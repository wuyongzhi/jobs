// Copyright 2015 Alex Browne.  All rights reserved.
// Use of this source code is governed by the MIT
// license, which can be found in the LICENSE file.

package jobs

import (
	"fmt"
	"github.com/garyburd/redigo/redis"
	"time"
)

// Job represents a discrete piece of work to be done by a worker.
type Job struct {
	id       string
	data     []byte
	typ      *Type
	status   Status
	time     int64
	freq     int64
	priority int
	err      error
	retries  uint
	started  int64
	finished int64
	poolId   string
}

// Id returns the unique identifier used for the job. If the job has not yet
// been saved to the database, it may return an empty string.
func (j *Job) Id() string {
	return j.id
}

// Status returns the status of the job.
func (j *Job) Status() Status {
	return j.status
}

// Error returns the last error that arose during execution of the job. It is
// only non-nil if the job has failed at some point.
func (j *Job) Error() error {
	return j.err
}

// Started returns the time that the job started executing (in local time
// with nanosecond precision) or the zero time if the job has not started
// executing yet.
func (j *Job) Started() time.Time {
	return time.Unix(0, j.started).Local()
}

// Finished returns the time that the job finished executing (in local
// time with nanosecond precision) or the zero time if the job has not
// finished executing yet.
func (j *Job) Finished() time.Time {
	return time.Unix(0, j.finished).Local()
}

// Duration returns how long the job took to execute with nanosecond
// precision. I.e. the difference between j.Finished() and j.Started().
// It returns a duration of zero if the job has not finished yet.
func (j *Job) Duration() time.Duration {
	if j.Finished().IsZero() {
		return 0 * time.Second
	}
	return j.Finished().Sub(j.Started())
}

// key returns the key used for the hash in redis which stores all the
// fields for this job.
func (j *Job) key() string {
	return "jobs:" + j.id
}

// isRecurring returns true iff the job is recurring
func (j *Job) isRecurring() bool {
	return j.freq != 0
}

// nextTime returns the time (unix UTC with nanosecond precision) that the
// job should execute next, if it is a recurring job, and 0 if it is not.
func (j *Job) nextTime() int64 {
	if !j.isRecurring() {
		return 0
	}
	// NOTE: is this the proper way to handle rescheduling?
	// What if we schedule jobs faster than they can be executed?
	// Should we just let them build up and expect the end user to
	// allocate more workers? Or should we schedule for time.Now at
	// the earliest to prevent buildup?
	return j.time + j.freq
}

// save writes the job to the database and adds it to the appropriate indexes and status
// sets, but does not enqueue it. If you want to add it to the queue, use the enqueue method
// after save.
func (j *Job) save() error {
	t := newTransaction()
	t.saveJob(j)
	if err := t.exec(); err != nil {
		return err
	}
	return nil
}

// saveJob adds commands to the transaction to set all the fields for the main hash for the job,
// add the job to the time index, move the job to the appropriate status set. It will
// also mutate the job by 1) generating an id if the id is empty and 2) setting the status to
// StatusSaved if the status is empty.
func (t *transaction) saveJob(job *Job) {
	// Generate id if needed
	if job.id == "" {
		job.id = generateRandomId()
	}
	// Set status to saved if needed
	if job.status == "" {
		job.status = StatusSaved
	}
	// Add the job attributes to a hash
	t.command("HMSET", job.mainHashArgs(), nil)
	// Add the job to the appropriate status set
	t.setStatus(job, job.status)
	// Add the job to the time index
	t.addJobToTimeIndex(job)
}

// addJobToTimeIndex adds commands to the transaction which will, when executed,
// add the job id to the time index with a score equal to the job's time field.
func (t *transaction) addJobToTimeIndex(job *Job) {
	t.command("ZADD", redis.Args{keys.jobsTimeIndex, job.time, job.id}, nil)
}

// Refresh mutates the job by setting its fields to the most recent data
// found in the database. It returns an error if there was a problem connecting
// to the database or if the job was destroyed.
func (j *Job) Refresh() error {
	t := newTransaction()
	t.scanJobById(j.id, j)
	if err := t.exec(); err != nil {
		return err
	}
	return nil
}

// enqueue adds the job to the queue and sets its status to StatusQueued. Queued jobs will
// be completed by workers in order of priority.
func (j *Job) enqueue() error {
	if err := j.setStatus(StatusQueued); err != nil {
		return err
	}
	return nil
}

// Reschedule reschedules the job with the given time. It can be used to reschedule
// cancelled jobs. It may also be used to reschedule finished or failed jobs, however,
// in most cases if you want to reschedule finished jobs you should use the ScheduleRecurring
// method and if you want to reschedule failed jobs, you should set the number of retries > 0
// when registering the job type. Reschedule returns an error if there was a problem connecting
// to the database.
func (j *Job) Reschedule(time time.Time) error {
	t := newTransaction()
	unixNanoTime := time.UTC().UnixNano()
	t.command("HSET", redis.Args{j.key(), "time", unixNanoTime}, nil)
	t.setStatus(j, StatusQueued)
	j.time = unixNanoTime
	t.addJobToTimeIndex(j)
	if err := t.exec(); err != nil {
		return err
	}
	j.status = StatusQueued
	return nil
}

// Cancel cancels the job, but does not remove it from the database. It will be
// added to a list of cancelled jobs. If you wish to remove it from the database,
// use the Destroy method.
func (j *Job) Cancel() error {
	if err := j.setStatus(StatusCancelled); err != nil {
		return err
	}
	return nil
}

// setError sets the err property of j and adds it to the set of jobs which had errors
func (j *Job) setError(err error) error {
	j.err = err
	t := newTransaction()
	t.command("HSET", redis.Args{j.key(), "error", j.err.Error()}, nil)
	if err := t.exec(); err != nil {
		return err
	}
	return nil
}

// Destroy removes all traces of the job from the database. If the job is currently
// being executed by a worker, the worker may still finish the job.
func (j *Job) Destroy() error {
	if j.id == "" {
		return fmt.Errorf("jobs: Cannot destroy job that doesn't have an id.")
	}
	// Start a new transaction
	t := newTransaction()
	// Call the script to destroy the job
	t.destroyJob(j)
	// Execute the transaction
	if err := t.exec(); err != nil {
		return err
	}
	j.status = StatusDestroyed
	return nil
}

// setStatus updates the job's status in the database and moves it to the appropriate
// status set.
func (j *Job) setStatus(status Status) error {
	if j.id == "" {
		return fmt.Errorf("jobs: Cannot set status to %s because job doesn't have an id.", status)
	}
	if j.status == StatusDestroyed {
		return fmt.Errorf("jobs: Cannot set job:%s status to %s because it was destroyed.", j.id, status)
	}
	// Use a transaction to move the job to the appropriate status set and set its status
	t := newTransaction()
	t.setStatus(j, status)
	if err := t.exec(); err != nil {
		return err
	}
	j.status = status
	return nil
}

// mainHashArgs returns the args for the hash which will store the job data
func (j *Job) mainHashArgs() []interface{} {
	hashArgs := []interface{}{j.key(),
		"data", string(j.data),
		"type", j.typ.name,
		"time", j.time,
		"freq", j.freq,
		"priority", j.priority,
		"retries", j.retries,
		"status", j.status,
		"started", j.started,
		"finished", j.finished,
		"poolId", j.poolId,
	}
	if j.err != nil {
		hashArgs = append(hashArgs, "error", j.err.Error())
	}
	return hashArgs
}

// scanJob scans the values of reply into job. reply should be the
// response of an HMGET or HGETALL query.
func scanJob(reply interface{}, job *Job) error {
	fields, err := redis.Values(reply, nil)
	if err != nil {
		return err
	}
	if len(fields)%2 != 0 {
		return fmt.Errorf("jobs: In scanJob: Expected length of fields to be even but got: %d", len(fields))
	}
	for i := 0; i < len(fields)-1; i += 2 {
		fieldName, err := redis.String(fields[i], nil)
		if err != nil {
			return fmt.Errorf("jobs: In scanJob: Could not convert fieldName (fields[%d] = %v) of type %T to string.", i, fields[i], fields[i])
		}
		fieldValue := fields[i+1]
		switch fieldName {
		case "id":
			if err := scanString(fieldValue, &(job.id)); err != nil {
				return err
			}
		case "data":
			if err := scanBytes(fieldValue, &(job.data)); err != nil {
				return err
			}
		case "type":
			typeName := ""
			if err := scanString(fieldValue, &typeName); err != nil {
				return err
			}
			Type, found := Types[typeName]
			if !found {
				return fmt.Errorf("jobs: In scanJob: Could not find Type with name = %s", typeName)
			}
			job.typ = Type
		case "time":
			if err := scanInt64(fieldValue, &(job.time)); err != nil {
				return err
			}
		case "freq":
			if err := scanInt64(fieldValue, &(job.freq)); err != nil {
				return err
			}
		case "priority":
			if err := scanInt(fieldValue, &(job.priority)); err != nil {
				return err
			}
		case "retries":
			if err := scanUint(fieldValue, &(job.retries)); err != nil {
				return err
			}
		case "status":
			status := ""
			if err := scanString(fieldValue, &status); err != nil {
				return err
			}
			job.status = Status(status)
		case "started":
			if err := scanInt64(fieldValue, &(job.started)); err != nil {
				return err
			}
		case "finished":
			if err := scanInt64(fieldValue, &(job.finished)); err != nil {
				return err
			}
		case "poolId":
			if err := scanString(fieldValue, &(job.poolId)); err != nil {
				return err
			}
		}
	}
	return nil
}

// scanInt converts a reply from redis into an int and scans the value into v.
func scanInt(reply interface{}, v *int) error {
	if v == nil {
		return fmt.Errorf("jobs: In scanInt: argument v was nil")
	}
	val, err := redis.Int(reply, nil)
	if err != nil {
		return fmt.Errorf("jobs: In scanInt: Could not convert %v of type %T to int.", reply, reply)
	}
	(*v) = val
	return nil
}

// scanUint converts a reply from redis into a uint and scans the value into v.
func scanUint(reply interface{}, v *uint) error {
	if v == nil {
		return fmt.Errorf("jobs: In scanUint: argument v was nil")
	}
	val, err := redis.Uint64(reply, nil)
	if err != nil {
		return fmt.Errorf("jobs: In scanUint: Could not convert %v of type %T to uint.", reply, reply)
	}
	(*v) = uint(val)
	return nil
}

// scanInt64 converts a reply from redis into an int64 and scans the value into v.
func scanInt64(reply interface{}, v *int64) error {
	if v == nil {
		return fmt.Errorf("jobs: In scanInt64: argument v was nil")
	}
	val, err := redis.Int64(reply, nil)
	if err != nil {
		return fmt.Errorf("jobs: In scanInt64: Could not convert %v of type %T to int64.", reply, reply)
	}
	(*v) = val
	return nil
}

// scanString converts a reply from redis into a string and scans the value into v.
func scanString(reply interface{}, v *string) error {
	if v == nil {
		return fmt.Errorf("jobs: In String: argument v was nil")
	}
	val, err := redis.String(reply, nil)
	if err != nil {
		return fmt.Errorf("jobs: In String: Could not convert %v of type %T to string.", reply, reply)
	}
	(*v) = val
	return nil
}

// scanBytes converts a reply from redis into a slice of bytes and scans the value into v.
func scanBytes(reply interface{}, v *[]byte) error {
	if v == nil {
		return fmt.Errorf("jobs: In scanBytes: argument v was nil")
	}
	val, err := redis.Bytes(reply, nil)
	if err != nil {
		return fmt.Errorf("jobs: In scanBytes: Could not convert %v of type %T to []byte.", reply, reply)
	}
	(*v) = val
	return nil
}

// scanJobById adds commands and a reply handler to the transaction which, when run,
// will scan the values of the job corresponding to id into job. It does not execute
// the transaction.
func (t *transaction) scanJobById(id string, job *Job) {
	job.id = id
	t.command("HGETALL", redis.Args{job.key()}, newScanJobHandler(job))
}

// FindById returns the job with the given id or an error if the job cannot be found
// or there was a problem connecting to the database.
func FindById(id string) (*Job, error) {
	job := &Job{}
	t := newTransaction()
	t.scanJobById(id, job)
	if err := t.exec(); err != nil {
		return nil, err
	}
	return job, nil
}
