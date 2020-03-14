package jobworker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-jwdk/jobworker/internal"
)

type Setting struct {
	Primary   Connector
	Secondary Connector

	DeadConnectorRetryInterval int64 // Seconds

	LoggerFunc LoggerFunc
}

var (
	ErrPrimaryConnIsRequired = errors.New("primary conn is required")
	ErrDuplicateEntryID      = errors.New("duplicate entry id")
)

func New(s *Setting) (*JobWorker, error) {

	if s.Primary == nil {
		return nil, ErrPrimaryConnIsRequired
	}

	var w JobWorker
	w.connProvider.Register(1, s.Primary)
	if s.Secondary != nil {
		w.connProvider.Register(2, s.Secondary)
	}
	w.connProvider.SetRetrySeconds(time.Duration(s.DeadConnectorRetryInterval) * time.Second)
	w.loggerFunc = s.LoggerFunc

	return &w, nil
}

type JobWorker struct {
	connProvider ConnectorProvider

	queue2worker map[string]*workerWithOption

	loggerFunc LoggerFunc

	started int32

	inShutdown  int32
	mu          sync.Mutex
	activeJob   map[*Job]struct{}
	activeJobWg sync.WaitGroup
	doneChan    chan struct{}
	heartBeat   internal.HeartBeat
	onShutdown  []func()
}

type LoggerFunc func(...interface{})

func (jw *JobWorker) Enqueue(ctx context.Context, input *EnqueueInput) (*EnqueueOutput, error) {
	for priority, conn := range jw.connProvider.GetConnectorsInPriorityOrder() {

		if jw.connProvider.IsDead(conn) {
			jw.debug("connector is dead. priority: ", priority)
			continue
		}

		_, err := conn.Enqueue(ctx, input)
		if err != nil {

			if err == ErrJobDuplicationDetected {
				jw.debug("skip enqueue a duplication job")
				return nil, nil
			}
			jw.debug("mark dead connector, because could not enqueue job. priority:", priority, "err:", err)
			jw.connProvider.MarkDead(conn)
			continue
		}
		return &EnqueueOutput{}, nil
	}
	return nil, errors.New("could not enqueue a job using all connector")
}

func (jw *JobWorker) EnqueueBatch(ctx context.Context, input *EnqueueBatchInput) (*EnqueueBatchOutput, error) {

	entryCnt := len(input.Entries)

	entrySet := make(map[string]struct{})
	for _, entry := range input.Entries {
		entrySet[entry.ID] = struct{}{}
	}

	if len(entrySet) < entryCnt {
		return nil, ErrDuplicateEntryID
	}

	var errs multiError
	for priority, conn := range jw.connProvider.GetConnectorsInPriorityOrder() {

		if jw.connProvider.IsDead(conn) {
			jw.debug("connector is dead. priority: ", priority)
			continue
		}

		var entries []*EnqueueBatchEntry
		for _, entry := range input.Entries {
			if _, ok := entrySet[entry.ID]; ok {
				entries = append(entries, entry)
			}
		}

		output, err := conn.EnqueueBatch(ctx, &EnqueueBatchInput{
			Queue:   input.Queue,
			Entries: entries,
		})

		if err != nil {
			jw.debug("could not batch enqueue job all. priority: ", priority, "error: ", err)
			errs.Errors = append(errs.Errors, err)
			continue
		}

		if len(output.Failed) == 0 {
			break
		}

		jw.debug("could not batch enqueue some job. priority: ", priority)
		jw.connProvider.MarkDead(conn)

		for _, id := range output.Successful {
			delete(entrySet, id)
		}
	}

	var out EnqueueBatchOutput
	for _, entry := range input.Entries {
		if _, failed := entrySet[entry.ID]; failed {
			out.Failed = append(out.Failed, entry.ID)
		} else {
			out.Successful = append(out.Successful, entry.ID)
		}
	}

	return &out, errs.ErrorOrNil()
}

type WorkerFunc func(job *Job) error

type Worker interface {
	Work(*Job) error
}

type defaultWorker struct {
	workFunc func(*Job) error
}

func (w *defaultWorker) Work(job *Job) error {
	return w.workFunc(job)
}

type Option struct {
	SubscribeMetadata map[string]string
}

type OptionFunc func(*Option)

func (o *Option) ApplyOptions(opts ...OptionFunc) {
	for _, opt := range opts {
		opt(o)
	}
}

// SubscribeMetadata is metadata of subscribe func
func SubscribeMetadata(k, v string) OptionFunc {
	return func(opt *Option) {
		if opt.SubscribeMetadata == nil {
			opt.SubscribeMetadata = make(map[string]string)
		}
		opt.SubscribeMetadata[k] = v
	}
}

func (jw *JobWorker) RegisterFunc(queue string, f WorkerFunc, opts ...OptionFunc) {
	jw.Register(queue, &defaultWorker{
		workFunc: f,
	}, opts...)
}

func (jw *JobWorker) Register(queue string, worker Worker, opts ...OptionFunc) {

	var opt Option
	opt.ApplyOptions(opts...)

	jw.mu.Lock()
	defer jw.mu.Unlock()
	if jw.queue2worker == nil {
		jw.queue2worker = make(map[string]*workerWithOption)
	}

	jw.queue2worker[queue] = &workerWithOption{
		worker: worker,
		opt:    &opt,
	}
}

type workerWithOption struct {
	worker Worker
	opt    *Option
}

type WorkSetting struct {
	HeartbeatInterval int64 // Sec
	OnHeartBeat       func(job *Job)
	WorkerConcurrency int
}

const (
	workerConcurrencyDefault = 1
)

func (s *WorkSetting) setDefaults() {
	if s.WorkerConcurrency == 0 {
		s.WorkerConcurrency = workerConcurrencyDefault
	}
}

var (
	ErrAlreadyStarted = errors.New("already started")
)

func (jw *JobWorker) Work(s *WorkSetting) error {

	if atomic.LoadInt32(&jw.started) == 1 {
		return ErrAlreadyStarted
	}
	atomic.StoreInt32(&jw.started, 1)

	s.setDefaults()

	var b internal.Broadcaster
	go func() {
		<-jw.getDoneChan()
		b.Broadcast()
	}()

	if s.HeartbeatInterval > 0 && s.OnHeartBeat != nil {
		interval := time.Duration(s.HeartbeatInterval) * time.Second
		_ = jw.heartBeat.Start(interval, jw.newActiveJobHandler(s.OnHeartBeat))
		b.Register(func() {
			_ = jw.heartBeat.Stop()
		})
	}

	trackedJobCh := make(chan *Job)
	for _, conn := range jw.connProvider.GetConnectorsInPriorityOrder() {
		for name, w := range jw.queue2worker {
			ctx := context.Background()
			output, err := conn.Subscribe(ctx, &SubscribeInput{
				Queue:    name,
				Metadata: w.opt.SubscribeMetadata,
			})
			if err != nil {
				return err
			}
			b.Register(func() {
				jw.debug("unsubscribe:", name)
				err := output.Subscription.UnSubscribe()
				if err != nil {
					jw.debug("an error occurred during unsubscribe:", name, err)
				}
			})

			go func(sub Subscription) {
				for job := range sub.Queue() {
					trackedJobCh <- job
					jw.trackJob(job, true)
				}
				jw.debug("Completed unsubscribe")
			}(output.Subscription)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < s.WorkerConcurrency; i++ {
		wg.Add(1)
		go func(id int) {
			sw := subWorker{id: strconv.Itoa(id), JobWorker: jw}
			sw.work(trackedJobCh)
			wg.Done()
		}(i)
	}

	wg.Wait()
	close(trackedJobCh)

	return nil

}

type subWorker struct {
	id string
	*JobWorker
}

func (sw *subWorker) work(jobs <-chan *Job) {
	for job := range jobs {
		sw.workSafely(context.Background(), job)
	}
}

func (jw *JobWorker) workSafely(ctx context.Context, job *Job) {

	connName := job.Conn.Name()

	jw.debug("start work safely:", connName, job.QueueName, job.Content)

	defer jw.trackJob(job, false)

	w, ok := jw.queue2worker[job.QueueName]
	if !ok {
		jw.debug("could not found queueName:", job.QueueName)
		return
	}

	if err := w.worker.Work(job); err != nil {
		if err = failJob(ctx, job); err != nil {
			jw.debug("mark dead connector, because error occurred during job fail:",
				connName, job.QueueName, job.Content, err)
			jw.connProvider.MarkDead(job.Conn)
		}
		return
	}
	if err := completeJob(ctx, job); err != nil {
		jw.debug("mark dead connector, because error occurred during job complete:",
			connName, job.QueueName, job.Content, err)
		jw.connProvider.MarkDead(job.Conn)
		return
	}
	jw.debug("success work safely:", connName, job.QueueName, job.Content)
}

func (jw *JobWorker) RegisterOnShutdown(f func()) {
	jw.mu.Lock()
	jw.onShutdown = append(jw.onShutdown, f)
	jw.mu.Unlock()
}

func (jw *JobWorker) Shutdown(ctx context.Context) error {
	atomic.StoreInt32(&jw.inShutdown, 1)

	jw.mu.Lock()
	jw.closeDoneChanLocked()
	for _, f := range jw.onShutdown {
		go f()
	}
	jw.mu.Unlock()

	finished := make(chan struct{}, 1)
	go func() {
		jw.activeJobWg.Wait()
		finished <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-finished:
		return nil
	}
}

const logPrefix = "[JWDK]"

func (jw *JobWorker) debug(args ...interface{}) {
	if jw.verbose() {
		args = append([]interface{}{logPrefix}, args...)
		jw.loggerFunc(args...)
	}
}

func (jw *JobWorker) verbose() bool {
	return jw.loggerFunc != nil
}

func (jw *JobWorker) newActiveJobHandler(handler func(job *Job)) func() {
	return func() {
		var jobs []*Job
		jw.mu.Lock()
		for v := range jw.activeJob {
			jobs = append(jobs, v)
		}
		jw.mu.Unlock()
		go func(jobs []*Job) {
			for _, job := range jobs {
				handler(job)
			}
		}(jobs)
	}
}

func (jw *JobWorker) shuttingDown() bool {
	return atomic.LoadInt32(&jw.inShutdown) != 0
}

func (jw *JobWorker) trackJob(job *Job, add bool) {
	jw.mu.Lock()
	defer jw.mu.Unlock()
	if jw.activeJob == nil {
		jw.activeJob = make(map[*Job]struct{})
	}
	if add {
		jw.activeJob[job] = struct{}{}
		jw.activeJobWg.Add(1)
	} else {
		delete(jw.activeJob, job)
		jw.activeJobWg.Done()
	}
	jw.debug("active job size:", len(jw.activeJob))
}

func (jw *JobWorker) getDoneChan() <-chan struct{} {
	jw.mu.Lock()
	defer jw.mu.Unlock()
	return jw.getDoneChanLocked()
}

func (jw *JobWorker) getDoneChanLocked() chan struct{} {
	if jw.doneChan == nil {
		jw.doneChan = make(chan struct{})
	}
	return jw.doneChan
}

func (jw *JobWorker) closeDoneChanLocked() {
	ch := jw.getDoneChanLocked()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func completeJob(ctx context.Context, job *Job) error {
	if job.IsFinished() {
		return nil
	}
	_, err := job.Conn.CompleteJob(ctx, &CompleteJobInput{Job: job})
	if err != nil {
		return err
	}
	job.finished()
	return nil
}

func failJob(ctx context.Context, job *Job) error {
	if job.IsFinished() {
		return nil
	}
	_, err := job.Conn.FailJob(ctx, &FailJobInput{Job: job})
	if err != nil {
		return err
	}
	job.finished()
	return nil
}

type multiError struct {
	Errors []error
}

func (e *multiError) Error() string {
	if len(e.Errors) == 1 {
		return fmt.Sprintf("1 error occurred:\n\t* %s\n\n", e.Errors[0])
	}

	points := make([]string, len(e.Errors))
	for i, err := range e.Errors {
		points[i] = fmt.Sprintf("* %s", err)
	}

	return fmt.Sprintf(
		"%d errors occurred:\n\t%s\n\n",
		len(e.Errors), strings.Join(points, "\n\t"))
}

func (e *multiError) ErrorOrNil() error {
	if e == nil {
		return nil
	}
	if len(e.Errors) == 0 {
		return nil
	}

	return e
}
