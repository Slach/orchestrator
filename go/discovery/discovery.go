/*
   Copyright 2014 Outbrain Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

/*

package discovery manages a queue of discovery requests: an ordered
queue with no duplicates.

The configured processor function is called on an entry that's
received and this happens in parallel until maxCapacity go-routines
are running in parallel.

Any new requests get queued and processed as a go routine becomes
free.  The queue is processed in FIFO order.  If a request is
received for an instance that is already in the queue or already
being processed then it will be silently ignored.

*/

package discovery

import (
	"sync"

	"github.com/outbrain/golib/log"
	"github.com/outbrain/orchestrator/go/inst"
)

// Queue is a container for processing the orchestrator discovery requests.
type Queue struct {
	// The number of active go routines processing discovery requests.
	concurrency uint
	// Channel used by the active go routines to say they have finished processing.
	done chan inst.InstanceKey
	// Input channel we are reading the discovery requests from.
	inputChan <-chan inst.InstanceKey
	// This has 2 uses: to indicate if there is a request in the queue
	// (so not being processed) or to indicate that the request is actively
	// being dealth with.  That state is not explicitly stored as it is not
	// really needed.
	knownKeys map[inst.InstanceKey]bool
	// lock while making changes
	lock sync.Mutex
	// The maximum number of go routines allowed to handle the queue at once.
	// This is a configuration parameter provided when creating the Queue.
	maxConcurrency uint
	// process to run on each received key
	processor func(i inst.InstanceKey)
	// This holds the discover requests (the instance name) which need to be
	// processed. but which are not currently being processed. All requests
	// are initially added to the end of this queue, and then the top element
	// will be popped off if the number of active go routines (defined by
	// concurrency) is less than the maximum specified value at which point
	// it will be processed by a new go routine.
	queue []inst.InstanceKey
}

var emptyKey = inst.InstanceKey{}

// NewQueue creates a new Queue entry.
func NewQueue(maxConcurrency uint, inputChan chan inst.InstanceKey, processor func(i inst.InstanceKey)) *Queue {
	q := new(Queue)

	q.concurrency = 0                    // explicitly
	q.done = make(chan inst.InstanceKey) // Do I need this to be larger?
	q.inputChan = inputChan
	q.knownKeys = make(map[inst.InstanceKey]bool)
	q.maxConcurrency = maxConcurrency
	q.processor = processor
	q.queue = make([]inst.InstanceKey, 0)

	return q
}

// add the key to the slice if it does not exist in known keys
// - goroutine safe as only called inside the mutex
func (q *Queue) push(key inst.InstanceKey) {
	if _, found := q.knownKeys[key]; !found {
		// add to the items that are being processed
		q.knownKeys[key] = true
		q.queue = append(q.queue, key)
	} else {
		// If key already there we just ignore it as the request is in the queue.
		// the known key also records stuff in the queue, so pending + active jobs.
	}
}

// remove the entry and remove it from known keys
func (q *Queue) pop() inst.InstanceKey {
	if len(q.queue) == 0 {
		log.Fatal("q.pop() called on empty queue")
	}
	key := q.queue[0]
	q.queue = q.queue[1:]
	delete(q.knownKeys, key)
	return key
}

// dispatch a job from the queue (assumes we are in a locked state)
func (q *Queue) dispatch() {
	key := q.pop()

	q.concurrency++
	q.knownKeys[key] = true

	// dispatch a discoverInstance() but tell us when we're done (to limit concurrency)
	go func() { // discover asynchronously
		q.processor(key)
		q.done <- key
	}()
}

// acknowledge a job has finished
// - we deal with the locking inside
func (q *Queue) acknowledgeJob(key inst.InstanceKey) {
	q.lock.Lock()
	delete(q.knownKeys, key)
	q.concurrency--
	q.lock.Unlock()
}

// drain queue by dispatching any jobs we have still
func (q *Queue) maybeDispatch() {
	q.lock.Lock()
	if q.concurrency < q.maxConcurrency && len(q.queue) > 0 {
		q.dispatch()
	}
	q.lock.Unlock()
}

// add an entry to the queue and dispatch something if concurrency is low enough
// - we deal with locking inside
func (q *Queue) queueAndMaybeDispatch(key inst.InstanceKey) {
	q.lock.Lock()
	q.push(key)
	if q.concurrency < q.maxConcurrency && len(q.queue) > 0 {
		q.dispatch()
	}
	q.lock.Unlock()
}

// cleanup is called when the input channel closes.
// we can not sit in the loop so we have to wait for running go-routines to finish
// but also to dispatch anything left in the queue until finally everything is done.
func (q *Queue) cleanup() {
	for q.concurrency > 0 && len(q.queue) > 0 {
		q.maybeDispatch()
		key := <-q.done
		q.acknowledgeJob(key)
	}
}

// Ends when all elements in the queue have been handled.
// we read from inputChan and call processor up to maxConcurrency times in parallel
func (q *Queue) HandleRequests() {
	for {
		select {
		case key, ok := <-q.inputChan:
			if !ok {
				q.cleanup()
				return
			}
			if key == emptyKey {
				log.Warningf("Queue.HandleRequests() q.inputChan received empty key %+v,"+
					"ignoring (fix the upstream code to prevent this)", key)
				break
			}
			q.queueAndMaybeDispatch(key)
		case key := <-q.done:
			q.acknowledgeJob(key)
		}
	}
}
