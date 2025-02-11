package worker_watcher //nolint:stylecheck

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spiral/errors"
	"github.com/spiral/roadrunner/v2/events"
	"github.com/spiral/roadrunner/v2/utils"
	"github.com/spiral/roadrunner/v2/worker"
	"github.com/spiral/roadrunner/v2/worker_watcher/container/channel"
)

// Vector interface represents vector container
type Vector interface {
	// Push used to put worker to the vector
	Push(worker.BaseProcess)
	// Pop used to get worker from the vector
	Pop(ctx context.Context) (worker.BaseProcess, error)
	// Remove worker with provided pid
	Remove(pid int64)
	// Destroy used to stop releasing the workers
	Destroy()

	// TODO Add Replace method, and remove `Remove` method. Replace will do removal and allocation
	// Replace(prevPid int64, newWorker worker.BaseProcess)
}

type workerWatcher struct {
	sync.RWMutex
	container Vector
	// used to control Destroy stage (that all workers are in the container)
	numWorkers *uint64

	workers []worker.BaseProcess

	allocator       worker.Allocator
	allocateTimeout time.Duration
	events          events.Handler
}

// NewSyncWorkerWatcher is a constructor for the Watcher
func NewSyncWorkerWatcher(allocator worker.Allocator, numWorkers uint64, events events.Handler, allocateTimeout time.Duration) *workerWatcher {
	ww := &workerWatcher{
		container: channel.NewVector(numWorkers),

		// pass a ptr to the number of workers to avoid blocking in the TTL loop
		numWorkers:      utils.Uint64(numWorkers),
		allocateTimeout: allocateTimeout,
		workers:         make([]worker.BaseProcess, 0, numWorkers),

		allocator: allocator,
		events:    events,
	}

	return ww
}

func (ww *workerWatcher) Watch(workers []worker.BaseProcess) error {
	for i := 0; i < len(workers); i++ {
		ww.container.Push(workers[i])
		// add worker to watch slice
		ww.workers = append(ww.workers, workers[i])

		go func(swc worker.BaseProcess) {
			ww.wait(swc)
		}(workers[i])
	}
	return nil
}

// Take is not a thread safe operation
func (ww *workerWatcher) Take(ctx context.Context) (worker.BaseProcess, error) {
	const op = errors.Op("worker_watcher_get_free_worker")

	// thread safe operation
	w, err := ww.container.Pop(ctx)
	if err != nil {
		if errors.Is(errors.WatcherStopped, err) {
			return nil, errors.E(op, errors.WatcherStopped)
		}

		return nil, errors.E(op, err)
	}

	// fast path, worker not nil and in the ReadyState
	if w.State().Value() == worker.StateReady {
		return w, nil
	}

	// =========================================================
	// SLOW PATH
	_ = w.Kill()
	// no free workers in the container or worker not in the ReadyState (TTL-ed)
	// try to continuously get free one
	for {
		w, err = ww.container.Pop(ctx)
		if err != nil {
			if errors.Is(errors.WatcherStopped, err) {
				return nil, errors.E(op, errors.WatcherStopped)
			}
			return nil, errors.E(op, err)
		}

		if err != nil {
			return nil, errors.E(op, err)
		}

		switch w.State().Value() {
		// return only workers in the Ready state
		// check first
		case worker.StateReady:
			return w, nil
		case worker.StateWorking: // how??
			ww.container.Push(w) // put it back, let worker finish the work
			continue
		case
			// all the possible wrong states
			worker.StateInactive,
			worker.StateDestroyed,
			worker.StateErrored,
			worker.StateStopped,
			worker.StateInvalid,
			worker.StateKilling,
			worker.StateStopping:
			// worker doing no work because it in the container
			// so we can safely kill it (inconsistent state)
			_ = w.Kill()
			// try to get new worker
			continue
		}
	}
}

func (ww *workerWatcher) Allocate() error {
	const op = errors.Op("worker_watcher_allocate_new")

	sw, err := ww.allocator()
	if err != nil {
		// log incident
		ww.events.Push(
			events.WorkerEvent{
				Event:   events.EventWorkerError,
				Payload: errors.E(op, errors.Errorf("can't allocate worker: %v", err)),
			})

		// if no timeout, return error immediately
		if ww.allocateTimeout == 0 {
			return errors.E(op, errors.WorkerAllocate, err)
		}

		// every half of a second
		allocateFreq := time.NewTicker(time.Millisecond * 500)

		tt := time.After(ww.allocateTimeout)
		for {
			select {
			case <-tt:
				// reduce number of workers
				atomic.AddUint64(ww.numWorkers, ^uint64(0))
				allocateFreq.Stop()
				// timeout exceed, worker can't be allocated
				return errors.E(op, errors.WorkerAllocate, err)

			case <-allocateFreq.C:
				sw, err = ww.allocator()
				if err != nil {
					// log incident
					ww.events.Push(
						events.WorkerEvent{
							Event:   events.EventWorkerError,
							Payload: errors.E(op, errors.Errorf("can't allocate worker, retry attempt failed: %v", err)),
						})
					continue
				}

				// reallocated
				allocateFreq.Stop()
				goto done
			}
		}
	}

done:
	// add worker to Wait
	ww.addToWatch(sw)

	ww.Lock()
	// add new worker to the workers slice (to get information about workers in parallel)
	ww.workers = append(ww.workers, sw)
	ww.Unlock()

	// push the worker to the container
	ww.Release(sw)
	return nil
}

// Remove worker
func (ww *workerWatcher) Remove(wb worker.BaseProcess) {
	ww.Lock()
	defer ww.Unlock()

	// set remove state
	pid := wb.Pid()

	// worker will be removed on the Get operation
	for i := 0; i < len(ww.workers); i++ {
		if ww.workers[i].Pid() == pid {
			ww.workers = append(ww.workers[:i], ww.workers[i+1:]...)
			// kill worker, just to be sure it's dead
			_ = wb.Kill()
			return
		}
	}
}

// Release O(1) operation
func (ww *workerWatcher) Release(w worker.BaseProcess) {
	switch w.State().Value() {
	case worker.StateReady:
		ww.container.Push(w)
	default:
		_ = w.Kill()
	}
}

// Destroy all underlying container (but let them complete the task)
func (ww *workerWatcher) Destroy(_ context.Context) {
	// destroy container, we don't use ww mutex here, since we should be able to push worker
	ww.Lock()
	// do not release new workers
	ww.container.Destroy()
	ww.Unlock()

	tt := time.NewTicker(time.Millisecond * 100)
	defer tt.Stop()
	for { //nolint:gosimple
		select {
		case <-tt.C:
			ww.Lock()
			// that might be one of the workers is working
			if atomic.LoadUint64(ww.numWorkers) != uint64(len(ww.workers)) {
				ww.Unlock()
				continue
			}
			// All container at this moment are in the container
			// Pop operation is blocked, push can't be done, since it's not possible to pop
			for i := 0; i < len(ww.workers); i++ {
				ww.workers[i].State().Set(worker.StateDestroyed)
				// kill the worker
				_ = ww.workers[i].Kill()
			}
			return
		}
	}
}

// List - this is O(n) operation, and it will return copy of the actual workers
func (ww *workerWatcher) List() []worker.BaseProcess {
	ww.RLock()
	defer ww.RUnlock()

	if len(ww.workers) == 0 {
		return nil
	}

	base := make([]worker.BaseProcess, 0, len(ww.workers))
	for i := 0; i < len(ww.workers); i++ {
		base = append(base, ww.workers[i])
	}

	return base
}

func (ww *workerWatcher) wait(w worker.BaseProcess) {
	const op = errors.Op("worker_watcher_wait")
	err := w.Wait()
	if err != nil {
		ww.events.Push(events.WorkerEvent{
			Event:   events.EventWorkerError,
			Worker:  w,
			Payload: errors.E(op, err),
		})
	}

	// remove worker
	ww.Remove(w)

	if w.State().Value() == worker.StateDestroyed {
		// worker was manually destroyed, no need to replace
		ww.events.Push(events.PoolEvent{Event: events.EventWorkerDestruct, Payload: w})
		return
	}

	// set state as stopped
	w.State().Set(worker.StateStopped)

	err = ww.Allocate()
	if err != nil {
		ww.events.Push(events.PoolEvent{
			Event: events.EventWorkerProcessExit,
			Error: errors.E(op, err),
		})

		// no workers at all, panic
		if len(ww.workers) == 0 && atomic.LoadUint64(ww.numWorkers) == 0 {
			panic(errors.E(op, errors.WorkerAllocate, errors.Errorf("can't allocate workers: %v", err)))
		}
	}
}

func (ww *workerWatcher) addToWatch(wb worker.BaseProcess) {
	go func() {
		ww.wait(wb)
	}()
}
