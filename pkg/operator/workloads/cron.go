/*
Copyright 2019 Cortex Labs, Inc.

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

package workloads

import (
	"time"

	kcore "k8s.io/api/core/v1"
	kmeta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/k8s"
	"github.com/cortexlabs/cortex/pkg/lib/sets/strset"
	"github.com/cortexlabs/cortex/pkg/lib/telemetry"
	"github.com/cortexlabs/cortex/pkg/operator/config"
)

const (
	_cronInterval      = 5 * time.Second
	_telemetryInterval = 1 * time.Hour
)

var _lastTelemetryCron time.Time

var cronChannel = make(chan struct{}, 1)

func cronRunner() {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-cronChannel:
			runCron()
		case <-timer.C:
			runCron()
		}
		timer.Reset(_cronInterval)
	}
}

func runCronNow() {
	cronChannel <- struct{}{}
}

func runCron() {
	defer reportAndRecover("cron failed")

	if err := UpdateWorkflows(); err != nil {
		telemetry.Error(err)
		errors.PrintError(err)
	}

	apiPods, err := config.Kubernetes.ListPodsByLabels(map[string]string{
		"workloadType": workloadTypeAPI,
		"userFacing":   "true",
	})

	if err != nil {
		telemetry.Error(err)
		errors.PrintError(err)
	}

	if err := updateAPISavedStatuses(apiPods); err != nil {
		telemetry.Error(err)
		errors.PrintError(err)
	}

	failedPods, err := config.Kubernetes.ListPods(&kmeta.ListOptions{
		FieldSelector: "status.phase=Failed",
	})

	if err != nil {
		telemetry.Error(err)
		errors.PrintError(err)
	}

	deleteEvictedPods(failedPods)

	if err := updateDataWorkloadErrors(failedPods); err != nil {
		telemetry.Error(err)
		errors.PrintError(err)
	}

	if time.Since(_lastTelemetryCron) >= _telemetryInterval {
		_lastTelemetryCron = time.Now()
		if err := telemetryCron(); err != nil {
			telemetry.Error(err)
			errors.PrintError(err)
		}
	}
}

func telemetryCron() error {
	nodes, err := config.Kubernetes.ListNodes(nil)
	if err != nil {
		return err
	}

	instanceTypeCounts := make(map[string]int)
	var totalInstances int

	for _, node := range nodes {
		if node.Labels["workload"] != "true" {
			continue
		}

		instanceType := node.Labels["beta.kubernetes.io/instance-type"]
		if instanceType == "" {
			instanceType = "unknown"
		}

		instanceTypeCounts[instanceType]++
		totalInstances++
	}

	properties := map[string]interface{}{
		"instanceTypes": instanceTypeCounts,
		"instanceCount": totalInstances,
	}

	telemetry.Event("operator.cron", properties)

	return nil
}

func reportAndRecover(strs ...string) error {
	if errInterface := recover(); errInterface != nil {
		err := errors.CastRecoverError(errInterface, strs...)
		telemetry.Error(err)
		errors.PrintError(err)
		return err
	}
	return nil
}

func deleteEvictedPods(failedPods []kcore.Pod) {
	evictedPods := []kcore.Pod{}
	for _, pod := range failedPods {
		if pod.Status.Reason == k8s.ReasonEvicted {
			evictedPods = append(evictedPods, pod)
		}
	}

	if len(evictedPods) > 0 {
		savedEvictedPods := map[string]kcore.Pod{}
		currentWorkloadIDs := strset.New()
		for _, ctx := range CurrentContexts() {
			currentWorkloadIDs.Merge(ctx.ComputedResourceWorkloadIDs())
		}

		for _, pod := range evictedPods {
			if currentWorkloadIDs.Has(pod.Labels["workloadID"]) {
				if _, ok := savedEvictedPods[pod.Labels["resourceID"]]; !ok {
					savedEvictedPods[pod.Labels["resourceID"]] = pod
					continue
				}
			}
			_, err := config.Kubernetes.DeletePod(pod.Name)
			if err != nil {
				errors.PrintError(err)
			}
		}
	}
}
