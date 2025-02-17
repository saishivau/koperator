// Copyright © 2022 Cisco Systems, Inc. and/or its affiliates
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"emperror.dev/errors"
	"github.com/go-logr/logr"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/banzaicloud/go-cruise-control/pkg/types"

	banzaiv1alpha1 "github.com/banzaicloud/koperator/api/v1alpha1"
	banzaiv1beta1 "github.com/banzaicloud/koperator/api/v1beta1"
	"github.com/banzaicloud/koperator/pkg/scale"
	"github.com/banzaicloud/koperator/pkg/util"
)

const (
	defaultFailedTasksHistoryMaxLength = 50
	ccOperationFinalizerGroup          = "finalizer.cruisecontroloperations.kafka.banzaicloud.io"
	ccOperationForStopExecution        = "ccOperationStopExecution"
	ccOperationFirstExecution          = "ccOperationFirstExecution"
	ccOperationRetryExecution          = "ccOperationRetryExecution"
	ccOperationInProgress              = "ccOperationInProgress"
)

var (
	defaultRequeueIntervalInSeconds = 10
	executionPriorityMap            = map[banzaiv1alpha1.CruiseControlTaskOperation]int{
		banzaiv1alpha1.OperationAddBroker:    2,
		banzaiv1alpha1.OperationRemoveBroker: 1,
		banzaiv1alpha1.OperationRebalance:    0,
	}
	missingCCResErr = errors.New("missing Cruise Control user task result")
)

// CruiseControlOperationReconciler reconciles CruiseControlOperation custom resources
type CruiseControlOperationReconciler struct {
	client.Client
	DirectClient client.Reader
	Scheme       *runtime.Scheme
	scaler       scale.CruiseControlScaler
	ScaleFactory func(ctx context.Context, kafkaCluster *banzaiv1beta1.KafkaCluster) (scale.CruiseControlScaler, error)
}

// +kubebuilder:rbac:groups=kafka.banzaicloud.io,resources=cruisecontroloperations,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=kafka.banzaicloud.io,resources=cruisecontroloperations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kafka.banzaicloud.io,resources=cruisecontroloperations/finalizers,verbs=update

//nolint:gocyclo
func (r *CruiseControlOperationReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx)
	log.V(1).Info("reconciling CruiseControlOperation custom resources")

	ccOperationListClusterWide := banzaiv1alpha1.CruiseControlOperationList{}
	err := r.DirectClient.List(ctx, &ccOperationListClusterWide, client.ListOption(client.InNamespace(request.Namespace)))
	if err != nil {
		return requeueWithError(log, err.Error(), err)
	}

	// Selecting the reconciling CruiseControlOperation
	var currentCCOperation *banzaiv1alpha1.CruiseControlOperation
	for i := range ccOperationListClusterWide.Items {
		ccOperation := &ccOperationListClusterWide.Items[i]
		if ccOperation.GetName() == request.Name && ccOperation.GetNamespace() == request.Namespace {
			currentCCOperation = ccOperation
			break
		}
	}

	if currentCCOperation == nil {
		return reconciled()
	}

	// When the task is done we can remove the finalizer instantly thus we can return fast here.
	if isFinalizerNeeded(currentCCOperation) && currentCCOperation.IsDone() {
		controllerutil.RemoveFinalizer(currentCCOperation, ccOperationFinalizerGroup)
		if err := r.Update(ctx, currentCCOperation); err != nil {
			return requeueWithError(log, "error happened when removing finalizer", err)
		}
		return reconciled()
	}

	// CruiseControlOperation has invalid operation type, reconciled
	if !currentCCOperation.IsCurrentTaskOperationValid() {
		log.Error(errors.New("Koperator does not support this operation"), "operation", currentCCOperation.CurrentTaskOperation())
		return reconciled()
	}

	kafkaClusterRef, err := kafkaClusterReference(currentCCOperation)
	if err != nil {
		return requeueWithError(log, "couldn't get kafka cluster reference", err)
	}

	kafkaCluster := &banzaiv1beta1.KafkaCluster{}
	err = r.Get(ctx, kafkaClusterRef, kafkaCluster)
	if err != nil {
		if apiErrors.IsNotFound(err) {
			if !currentCCOperation.ObjectMeta.DeletionTimestamp.IsZero() {
				log.Info("kafka cluster has been removed, now removing finalizer from CruiseControlOperation")
				controllerutil.RemoveFinalizer(currentCCOperation, ccOperationFinalizerGroup)
				if err := r.Update(ctx, currentCCOperation); err != nil {
					return requeueWithError(log, "failed to remove finalizer from CruiseControlOperation", err)
				}
			}
			return reconciled()
		}
		return requeueWithError(log, "failed to lookup referenced kafka cluster", err)
	}

	// Adding finalizer
	if err := r.addFinalizer(ctx, currentCCOperation); err != nil {
		return requeueWithError(log, "failed to add finalizer to CruiseControlOperation", err)
	}

	r.scaler, err = r.ScaleFactory(ctx, kafkaCluster)
	if err != nil {
		return requeueWithError(log, "failed to create Cruise Control Scaler instance", err)
	}

	// Checking Cruise Control health
	status, err := r.scaler.Status(ctx)
	if err != nil {
		log.Error(err, "could not get Cruise Control status")
		return requeueAfter(defaultRequeueIntervalInSeconds)
	}

	if !status.IsReady() {
		log.Info("requeue event as Cruise Control is not ready (yet)", "status", status)
		return requeueAfter(defaultRequeueIntervalInSeconds)
	}

	// Filtering out CruiseControlOperation by kafka cluster ref and state
	var ccOperationsKafkaClusterFiltered []*banzaiv1alpha1.CruiseControlOperation
	for i := range ccOperationListClusterWide.Items {
		operation := &ccOperationListClusterWide.Items[i]
		ref, err := kafkaClusterReference(operation)
		if err != nil {
			// Note: not returning here to continue processing the operations,
			// even if the user does not provide a KafkaClusterRef label on the CCOperation then the ref will be an empty object (not nil) and the filter will skip it.
			log.Info(err.Error())
		}
		if ref.Name == kafkaClusterRef.Name && ref.Namespace == kafkaClusterRef.Namespace &&
			operation.IsCurrentTaskOperationValid() && !operation.IsDone() {
			ccOperationsKafkaClusterFiltered = append(ccOperationsKafkaClusterFiltered, operation)
		}
	}

	// Update currentTask states from Cruise Control
	err = r.updateCurrentTasks(ctx, ccOperationsKafkaClusterFiltered)
	if err != nil {
		log.Error(err, "requeue event as updating state of currentTask(s) failed")
		return requeueAfter(defaultRequeueIntervalInSeconds)
	}

	// When the task is not in execution we can remove the finalizer
	if isFinalizerNeeded(currentCCOperation) && !currentCCOperation.IsCurrentTaskRunning() {
		controllerutil.RemoveFinalizer(currentCCOperation, ccOperationFinalizerGroup)
		if err := r.Update(ctx, currentCCOperation); err != nil {
			return requeueWithError(log, err.Error(), err)
		}
		return reconciled()
	}

	// Sorting operations into categories which are sorted by priority
	ccOperationQueueMap := sortOperations(ccOperationsKafkaClusterFiltered)

	// When there is no more job present in the cluster we reconciled.
	if len(ccOperationQueueMap[ccOperationForStopExecution]) == 0 && len(ccOperationQueueMap[ccOperationFirstExecution]) == 0 &&
		len(ccOperationQueueMap[ccOperationRetryExecution]) == 0 && len(ccOperationQueueMap[ccOperationInProgress]) == 0 {
		log.Info("there is no more operation for execution")
		return reconciled()
	}

	ccOperationExecution := selectOperationForExecution(ccOperationQueueMap)
	// There is nothing to be executed for now, requeue
	if ccOperationExecution == nil {
		return requeueAfter(defaultRequeueIntervalInSeconds)
	}

	// Check if CruiseControl is ready as we cannot perform any operation until it is in ready state unless it is a stop execution operation
	if (status.InExecution() || len(ccOperationQueueMap[ccOperationInProgress]) > 0) && ccOperationExecution.CurrentTaskOperation() != banzaiv1alpha1.OperationStopExecution {
		// Requeue because we can't do more
		return requeueAfter(defaultRequeueIntervalInSeconds)
	}

	log.Info("executing Cruise Control task", "operation", ccOperationExecution.CurrentTaskOperation(), "parameters", ccOperationExecution.CurrentTaskParameters())
	// Executing operation
	cruseControlTaskResult, err := r.executeOperation(ctx, ccOperationExecution)

	if err != nil {
		log.Error(err, "Cruise Control task execution got an error", "name", ccOperationExecution.GetName(), "namespace", ccOperationExecution.GetNamespace(), "operation", ccOperationExecution.CurrentTaskOperation(), "parameters", ccOperationExecution.CurrentTaskParameters())
		// This can happen when the CruiseControlOperation parameter is wrong
		if cruseControlTaskResult == nil {
			return requeueWithError(log, "CruiseControlOperation custom resource is invalid", err)
		}
	}

	conflictRetryFunction := func() error {
		if err = updateResult(log, cruseControlTaskResult, ccOperationExecution, true); err != nil {
			return err
		}
		err = r.Status().Update(ctx, ccOperationExecution)
		if apiErrors.IsConflict(err) {
			err = r.Get(ctx, client.ObjectKey{Name: ccOperationExecution.GetName(), Namespace: ccOperationExecution.GetNamespace()}, ccOperationExecution)
		}
		return err
	}
	// Protection to avoid re-execution task when status update is failed
	if err := util.RetryOnConflict(util.DefaultBackOffForConflict, conflictRetryFunction); err != nil {
		return requeueWithError(log, "could not update the result of the Cruise Control user task execution to the CruiseControlOperation status", err)
	}

	return reconciled()
}

func (r *CruiseControlOperationReconciler) addFinalizer(ctx context.Context, currentCCOperation *banzaiv1alpha1.CruiseControlOperation) error {
	// examine DeletionTimestamp to determine if object is under deletion
	if currentCCOperation.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(currentCCOperation, ccOperationFinalizerGroup) {
			controllerutil.AddFinalizer(currentCCOperation, ccOperationFinalizerGroup)
			if err := r.Update(ctx, currentCCOperation); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *CruiseControlOperationReconciler) executeOperation(ctx context.Context, ccOperationExecution *banzaiv1alpha1.CruiseControlOperation) (*scale.Result, error) {
	var cruseControlTaskResult *scale.Result
	var err error
	switch ccOperationExecution.CurrentTaskOperation() {
	case banzaiv1alpha1.OperationAddBroker:
		cruseControlTaskResult, err = r.scaler.AddBrokersWithParams(ctx, ccOperationExecution.CurrentTaskParameters())
	case banzaiv1alpha1.OperationRemoveBroker:
		cruseControlTaskResult, err = r.scaler.RemoveBrokersWithParams(ctx, ccOperationExecution.CurrentTaskParameters())
	case banzaiv1alpha1.OperationRebalance:
		cruseControlTaskResult, err = r.scaler.RebalanceWithParams(ctx, ccOperationExecution.CurrentTaskParameters())
	case banzaiv1alpha1.OperationStopExecution:
		cruseControlTaskResult, err = r.scaler.StopExecution(ctx)
	default:
		err = errors.NewWithDetails("Cruise Control operation not supported", "name", ccOperationExecution.GetName(), "namespace", ccOperationExecution.GetNamespace(), "operation", ccOperationExecution.CurrentTaskOperation(), "parameters", ccOperationExecution.CurrentTaskParameters())
	}
	return cruseControlTaskResult, err
}

func sortOperations(ccOperations []*banzaiv1alpha1.CruiseControlOperation) map[string][]*banzaiv1alpha1.CruiseControlOperation {
	ccOperationQueueMap := make(map[string][]*banzaiv1alpha1.CruiseControlOperation)
	for _, ccOperation := range ccOperations {
		switch {
		case isWaitingForFinalization(ccOperation):
			ccOperationQueueMap[ccOperationForStopExecution] = append(ccOperationQueueMap[ccOperationForStopExecution], ccOperation)
		case ccOperation.IsWaitingForFirstExecution():
			ccOperationQueueMap[ccOperationFirstExecution] = append(ccOperationQueueMap[ccOperationFirstExecution], ccOperation)
		case ccOperation.IsWaitingForRetryExecution():
			ccOperationQueueMap[ccOperationRetryExecution] = append(ccOperationQueueMap[ccOperationRetryExecution], ccOperation)
		case ccOperation.IsInProgress():
			ccOperationQueueMap[ccOperationInProgress] = append(ccOperationQueueMap[ccOperationInProgress], ccOperation)
		}
	}

	// Sorting by operation type and by the k8s object creation time
	for key := range ccOperationQueueMap {
		ccOperationQueue := ccOperationQueueMap[key]
		sort.SliceStable(ccOperationQueue, func(i, j int) bool {
			return executionPriorityMap[ccOperationQueue[i].CurrentTaskOperation()] > executionPriorityMap[ccOperationQueue[j].CurrentTaskOperation()] ||
				(executionPriorityMap[ccOperationQueue[i].CurrentTaskOperation()] == executionPriorityMap[ccOperationQueue[j].CurrentTaskOperation()] &&
					ccOperationQueue[i].CreationTimestamp.Unix() < ccOperationQueue[j].CreationTimestamp.Unix())
		})
	}
	return ccOperationQueueMap
}

func selectOperationForExecution(ccOperationQueueMap map[string][]*banzaiv1alpha1.CruiseControlOperation) *banzaiv1alpha1.CruiseControlOperation {
	// SELECTING OPERATION FOR EXECUTION
	var ccOperationExecution *banzaiv1alpha1.CruiseControlOperation
	// First prio: execute the finalize task
	switch {
	case len(ccOperationQueueMap[ccOperationForStopExecution]) > 0:
		ccOperationExecution = ccOperationQueueMap[ccOperationForStopExecution][0]
		ccOperationExecution.CurrentTask().Operation = banzaiv1alpha1.OperationStopExecution
	// Second prio: execute add_broker operation
	case len(ccOperationQueueMap[ccOperationFirstExecution]) > 0 && ccOperationQueueMap[ccOperationFirstExecution][0].CurrentTaskOperation() == banzaiv1alpha1.OperationAddBroker:
		ccOperationExecution = ccOperationQueueMap[ccOperationFirstExecution][0]
	// Third prio: execute failed task
	case len(ccOperationQueueMap[ccOperationRetryExecution]) > 0:
		// When the default backoff duration elapsed we retry
		if ccOperationQueueMap[ccOperationRetryExecution][0].IsReadyForRetryExecution() {
			ccOperationExecution = ccOperationQueueMap[ccOperationRetryExecution][0]
		}
	// Forth prio: execute the first element in the FirstExecutionQueue which is ordered by operation type and k8s creation timestamp
	case len(ccOperationQueueMap[ccOperationFirstExecution]) > 0:
		ccOperationExecution = ccOperationQueueMap[ccOperationFirstExecution][0]
	}

	return ccOperationExecution
}

// SetupCruiseControlWithManager registers cruise control controller to the manager
func SetupCruiseControlOperationWithManager(mgr ctrl.Manager) *ctrl.Builder {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&banzaiv1alpha1.CruiseControlOperation{}).
		WithEventFilter(SkipClusterRegistryOwnedResourcePredicate{}).
		Named("CruiseControlOperation")

	builder.WithEventFilter(
		predicate.Funcs{
			// We don't reconcile when there is no operation defined
			CreateFunc: func(e event.CreateEvent) bool {
				obj := e.Object.(*banzaiv1alpha1.CruiseControlOperation)
				// Doesn't need to reconcile when the operation is done and finalizing is not needed
				return !(obj.IsDone() && obj.GetDeletionTimestamp().IsZero()) && obj.CurrentTaskOperation() != ""
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldObj := e.ObjectOld.(*banzaiv1alpha1.CruiseControlOperation)
				newObj := e.ObjectNew.(*banzaiv1alpha1.CruiseControlOperation)
				// Doesn't need to reconcile when the operation is done and finalizing is not needed
				if newObj.IsDone() && newObj.GetDeletionTimestamp().IsZero() {
					return false
				}
				if !reflect.DeepEqual(oldObj.CurrentTask(), newObj.CurrentTask()) ||
					oldObj.GetDeletionTimestamp() != newObj.GetDeletionTimestamp() ||
					oldObj.IsPaused() != newObj.IsPaused() ||
					oldObj.GetGeneration() != newObj.GetGeneration() {
					return true
				}
				return false
			},
		})

	return builder
}

func isFinalizerNeeded(operation *banzaiv1alpha1.CruiseControlOperation) bool {
	return controllerutil.ContainsFinalizer(operation, ccOperationFinalizerGroup) && !operation.ObjectMeta.DeletionTimestamp.IsZero()
}

func kafkaClusterReference(operation *banzaiv1alpha1.CruiseControlOperation) (client.ObjectKey, error) {
	if operation.GetClusterRef() == "" {
		return client.ObjectKey{}, errors.NewWithDetails("could not find kafka cluster reference label for CruiseControlOperation", "missing label", banzaiv1beta1.KafkaCRLabelKey, "name", operation.GetName(), "namespace", operation.GetNamespace())
	}
	return client.ObjectKey{
		Name:      operation.GetClusterRef(),
		Namespace: operation.GetNamespace(),
	}, nil
}

func updateResult(log logr.Logger, res *scale.Result, operation *banzaiv1alpha1.CruiseControlOperation, isAfterExecution bool) error {
	// This can happen rarely when the max cached completed user tasks is reached
	if res == nil {
		log.Error(missingCCResErr, "Cruise Control's max.cached.completed.user.tasks configuration value probably too small. Missing user task state is handled as completedWithError", "name", operation.GetName(), "namespace", operation.GetNamespace(), "task ID", operation.CurrentTaskID())
		res = &scale.Result{
			TaskID: operation.CurrentTaskID(),
			State:  banzaiv1beta1.CruiseControlTaskCompletedWithError,
			Err:    missingCCResErr,
		}
	}

	operation.Status.ErrorPolicy = operation.Spec.ErrorPolicy
	task := operation.CurrentTask()

	if (res.State == banzaiv1beta1.CruiseControlTaskCompleted || res.State == banzaiv1beta1.CruiseControlTaskCompletedWithError) && task.Finished == nil {
		task.Finished = &v1.Time{Time: time.Now()}
	}

	// Add the failed task into the status.failedTasks slice only when the update is happened after executing the task
	if isAfterExecution && task.Finished != nil && task.State == banzaiv1beta1.CruiseControlTaskCompletedWithError {
		if len(operation.Status.FailedTasks) >= defaultFailedTasksHistoryMaxLength {
			operation.Status.FailedTasks = operation.Status.FailedTasks[1:]
		}
		operation.Status.FailedTasks = append(operation.Status.FailedTasks, *task)

		operation.Status.RetryCount += 1
		task.SetDefaults()
	}

	if isAfterExecution {
		if task.Started == nil {
			startTime, err := time.Parse(time.RFC1123, res.StartedAt)
			if err != nil {
				return errors.WrapIff(err, "could not parse user task start time from Cruise Control API")
			}
			task.Started = &v1.Time{Time: startTime}
		}
		task.ID = res.TaskID
		task.Summary = formatSummary(res.Result)
		if res.Err != nil {
			task.ErrorMessage = res.Err.Error()
		}
		task.HTTPRequest = res.RequestURL
		task.HTTPResponseCode = &res.ResponseStatusCode
	}

	task.State = res.State

	return nil
}

// updateCurrentTasks the state of the CruiseControlOperation from the CruiseControlTasksAndStates instance by getting their
// status from Cruise Control.
func (r *CruiseControlOperationReconciler) updateCurrentTasks(ctx context.Context, ccOperations []*banzaiv1alpha1.CruiseControlOperation) error {
	log := logr.FromContextOrDiscard(ctx)

	userTaskIDs := make([]string, 0, len(ccOperations))
	var ccOperationsCopy []*banzaiv1alpha1.CruiseControlOperation
	for _, ccOperation := range ccOperations {
		if ccOperation.CurrentTaskID() != "" {
			userTaskIDs = append(userTaskIDs, ccOperation.CurrentTaskID())
		}

		ccOperationsCopy = append(ccOperationsCopy, ccOperation.DeepCopy())
	}

	tasks, err := r.scaler.UserTasks(ctx, userTaskIDs...)
	if err != nil {
		return errors.WrapIff(err, "could not get user tasks from Cruise Control API")
	}

	taskResultsByID := make(map[string]*scale.Result, len(tasks))
	for _, task := range tasks {
		taskResultsByID[task.TaskID] = task
	}
	for i := range ccOperations {
		ccOperation := ccOperations[i]
		if ccOperation.CurrentTaskID() != "" && !ccOperation.IsDone() {
			if err := updateResult(log, taskResultsByID[ccOperation.CurrentTaskID()], ccOperation, false); err != nil {
				return errors.WrapWithDetails(err, "could not set Cruise Control user task result to CruiseControlOperation CurrentTask", "name", ccOperations[i].GetName(), "namespace", ccOperations[i].GetNamespace())
			}
		}
	}

	for i := range ccOperations {
		if !reflect.DeepEqual(ccOperations[i].Status, ccOperationsCopy[i].Status) {
			if err := r.Status().Update(ctx, ccOperations[i]); err != nil {
				return errors.WrapIfWithDetails(err, "could not update CruiseControlOperation status", "name", ccOperations[i].GetName(), "namespace", ccOperations[i].GetNamespace())
			}
		}
	}
	return nil
}

func isWaitingForFinalization(ccOperation *banzaiv1alpha1.CruiseControlOperation) bool {
	return ccOperation.IsCurrentTaskRunning() && !ccOperation.ObjectMeta.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(ccOperation, ccOperationFinalizerGroup)
}

func formatSummary(res *types.OptimizationResult) map[string]string {
	if res == nil {
		return nil
	}
	return map[string]string{
		"Data to move":                             fmt.Sprintf("%d", res.Summary.DataToMoveMB),
		"Number of replica movements":              fmt.Sprintf("%d", res.Summary.NumReplicaMovements),
		"Intra broker data to move":                fmt.Sprintf("%d", res.Summary.IntraBrokerDataToMoveMB),
		"Number of intra broker replica movements": fmt.Sprintf("%d", res.Summary.NumIntraBrokerReplicaMovements),
		"Number of leader movements":               fmt.Sprintf("%d", res.Summary.NumLeaderMovements),
		"Recent windows":                           fmt.Sprintf("%d", res.Summary.RecentWindows),
		"Provision recommendation":                 res.Summary.ProvisionRecommendation,
	}
}
