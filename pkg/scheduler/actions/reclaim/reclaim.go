/*
Copyright 2018 The Kubernetes Authors.

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

package reclaim

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/util"
)

type Action struct{}

func New() *Action {
	return &Action{}
}

func (ra *Action) Name() string {
	return "reclaim"
}

func (ra *Action) Initialize() {}

func (ra *Action) Execute(ssn *framework.Session) {
	klog.V(5).Infof("Enter Reclaim ...")
	defer klog.V(5).Infof("Leaving Reclaim ...")

	queues, queueMap, preemptorsMap, preemptorTasks := ra.initReclaimStructures(ssn)
	ra.filterReclaimableJobs(ssn, queues, queueMap, preemptorsMap, preemptorTasks)

	ra.reclaimResources(ssn, queues, preemptorsMap, preemptorTasks)
}

// reclaim stages,do it in order:
// 1. filter out the unoverused queue and choose preemptor task
// 2. Range over all nodes and run predicateFn for preemptor task
// 3. collect reclaimable tasks
// 4. evicted vitim tasks
func (ra *Action) reclaimResources(ssn *framework.Session, queues *util.PriorityQueue, preemptorsMap map[api.QueueID]*util.PriorityQueue, preemptorTasks map[api.JobID]*util.PriorityQueue) {
	for !queues.Empty() {
		var job *api.JobInfo
		var task *api.TaskInfo

		queue := queues.Pop().(*api.QueueInfo)
		if ssn.Overused(queue) {
			klog.V(3).Infof("Queue <%s> is overused, ignore it.", queue.Name)
			continue
		}
		// Found "high" priority job
		jobs, found := preemptorsMap[queue.UID]
		if !found || jobs.Empty() {
			continue
		} else {
			job = jobs.Pop().(*api.JobInfo)
		}
		// Found "high" priority task to reclaim others
		if tasks, found := preemptorTasks[job.UID]; !found || tasks.Empty() || !ssn.JobStarving(job) {
			continue
		} else {
			task = tasks.Pop().(*api.TaskInfo)
		}
		if task.Pod.Spec.PreemptionPolicy != nil && *task.Pod.Spec.PreemptionPolicy == v1.PreemptNever {
			klog.V(3).Infof("Task %s/%s is not eligible to preempt other tasks due to preemptionPolicy is Never", task.Namespace, task.Name)
			jobs.Push(job)
			queues.Push(queue)
			continue
		}
		//In allocate action we need check all the ancestor queues' capability but in reclaim action we should just check current queue's capability, and reclaim happens when queue not allocatable so we just need focus on the reclaim here.
		//So it's more descriptive to user preempt related semantics.
		if !ssn.Preemptive(queue, task) {
			klog.V(3).Infof("Queue <%s> can not reclaim by preempt others when considering task <%s> , ignore it.", queue.Name, task.Name)
			continue
		}
		if err := ssn.PrePredicateFn(task); err != nil {
			klog.V(3).Infof("PrePredicate for task %s/%s failed for: %v", task.Namespace, task.Name, err)
			continue
		}
		ra.reclaimFromNode(ssn, task, job)
		jobs.Push(job)
		queues.Push(queue)
	}
}

// check out nodes to collection reclaimees
func (ra *Action) reclaimFromNode(ssn *framework.Session, task *api.TaskInfo, job *api.JobInfo) {
	// we should filter out those nodes that are UnschedulableAndUnresolvable status got in allocate action
	totalNodes := ssn.GetUnschedulableAndUnresolvableNodesForTask(task)
	for _, node := range totalNodes {
		// When filtering candidate nodes, need to consider the node statusSets instead of the err information.
		// refer to kube-scheduler preemption code: https://github.com/kubernetes/kubernetes/blob/9d87fa215d9e8020abdc17132d1252536cd752d2/pkg/scheduler/framework/preemption/preemption.go#L422
		if err := ssn.PredicateForPreemptAction(task, node); err != nil {
			klog.V(4).Infof("Reclaim predicate for task %s/%s on node %s return error %v ", task.Namespace, task.Name, node.Name, err)
			continue
		}

		klog.V(3).Infof("Considering Task <%s/%s> on Node <%s>.", task.Namespace, task.Name, node.Name)

		reclaimees := ra.findReclaimableTasks(ssn, node, job)
		if len(reclaimees) == 0 {
			klog.V(4).Infof("No reclaimees on Node <%s>.", node.Name)
			continue
		}
		ra.collectAndProcessVitimTasks(ssn, node, task, reclaimees)
	}
}

func (ra *Action) findReclaimableTasks(ssn *framework.Session, node *api.NodeInfo, job *api.JobInfo) []*api.TaskInfo {
	var reclaimees []*api.TaskInfo
	for _, task := range node.Tasks {
		// Ignore non running task.
		if task.Status != api.Running {
			continue
		}
		if !task.Preemptable {
			continue
		}
		if j, found := ssn.Jobs[task.Job]; !found {
			continue
		} else if j.Queue != job.Queue {
			q := ssn.Queues[j.Queue]
			if !q.Reclaimable() {
				continue
			}
			// Clone task to avoid modify Task's status on node.
			reclaimees = append(reclaimees, task.Clone())
		}
	}
	return reclaimees
}

func (ra *Action) collectAndProcessVitimTasks(ssn *framework.Session, node *api.NodeInfo, task *api.TaskInfo, reclaimees []*api.TaskInfo) {
	victims := ssn.Reclaimable(task, reclaimees)

	if err := util.ValidateVictims(task, node, victims); err != nil {
		klog.V(3).Infof("No validated victims on Node <%s>: %v", node.Name, err)
		return
	}
	victimsQueue := ssn.BuildVictimsPriorityQueue(victims, task)

	resreq := task.InitResreq.Clone()
	reclaimed := api.EmptyResource()

	// Reclaim victims for tasks.
	for !victimsQueue.Empty() {
		reclaimee := victimsQueue.Pop().(*api.TaskInfo)
		klog.Errorf("Try to reclaim Task <%s/%s> for Tasks <%s/%s>",
			reclaimee.Namespace, reclaimee.Name, task.Namespace, task.Name)
		if err := ssn.Evict(reclaimee, "reclaim"); err != nil {
			klog.Errorf("Failed to reclaim Task <%s/%s> for Tasks <%s/%s>: %v",
				reclaimee.Namespace, reclaimee.Name, task.Namespace, task.Name, err)
			continue
		}
		reclaimed.Add(reclaimee.Resreq)
		// If reclaimed enough resources, break loop to avoid Sub panic.
		if resreq.LessEqual(reclaimed, api.Zero) {
			break
		}
	}

	klog.V(3).Infof("Reclaimed <%v> for task <%s/%s> requested <%v>.",
		reclaimed, task.Namespace, task.Name, task.InitResreq)
	if task.InitResreq.LessEqual(reclaimed, api.Zero) {
		if err := ssn.Pipeline(task, node.Name); err != nil {
			klog.Errorf("Failed to pipeline Task <%s/%s> on Node <%s>",
				task.Namespace, task.Name, node.Name)
		}
		return
	}
}

func (ra *Action) initReclaimStructures(ssn *framework.Session) (
	*util.PriorityQueue,
	map[api.QueueID]*api.QueueInfo,
	map[api.QueueID]*util.PriorityQueue,
	map[api.JobID]*util.PriorityQueue) {
	return util.NewPriorityQueue(ssn.QueueOrderFn),
		make(map[api.QueueID]*api.QueueInfo),
		make(map[api.QueueID]*util.PriorityQueue),
		make(map[api.JobID]*util.PriorityQueue)
}

// in order to filter out the pending tasks and update local objects
func (ra *Action) filterReclaimableJobs(ssn *framework.Session, queues *util.PriorityQueue, queueMap map[api.QueueID]*api.QueueInfo, preemptorsMap map[api.QueueID]*util.PriorityQueue, preemptorTasks map[api.JobID]*util.PriorityQueue) {
	for _, job := range ssn.Jobs {
		if job.IsPending() {
			continue
		}
		if vr := ssn.JobValid(job); vr != nil && !vr.Pass {
			klog.V(4).Infof("Job <%s/%s> Queue <%s> skip reclaim, reason: %v, message %v", job.Namespace, job.Name, job.Queue, vr.Reason, vr.Message)
			continue
		}
		if queue, found := ssn.Queues[job.Queue]; !found {
			klog.Errorf("Failed to find Queue <%s> for Job <%s/%s>", job.Queue, job.Namespace, job.Name)
			continue
		} else if _, existed := queueMap[queue.UID]; !existed {
			klog.V(4).Infof("Added Queue <%s> for Job <%s/%s>", queue.Name, job.Namespace, job.Name)
			queueMap[queue.UID] = queue
			queues.Push(queue)
		}
		if ssn.JobStarving(job) {
			if _, found := preemptorsMap[job.Queue]; !found {
				preemptorsMap[job.Queue] = util.NewPriorityQueue(ssn.JobOrderFn)
			}
			preemptorsMap[job.Queue].Push(job)
			preemptorTasks[job.UID] = util.NewPriorityQueue(ssn.TaskOrderFn)
			for _, task := range job.TaskStatusIndex[api.Pending] {
				if task.SchGated {
					continue
				}
				preemptorTasks[job.UID].Push(task)
			}
		}
	}
}

func (ra *Action) UnInitialize() {
}
