/*
Copyright 2020 The KubeDiag Authors.

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

package eventer

import (
	"context"
	"fmt"
	"regexp"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	diagnosisv1 "github.com/kubediag/kubediag/api/v1"
	"github.com/kubediag/kubediag/pkg/util"
)

var (
	// KubernetesEventGeneratedDiagnosisPrefix is the name prefix for diagnoses generated by kubernetes events.
	KubernetesEventGeneratedDiagnosisPrefix = "kubernetes-event"
	// KubernetesEventAnnotation is the annotation used to store the kubernetes event that triggers a diagnosis.
	KubernetesEventAnnotation = util.KubeDiagPrefix + KubernetesEventGeneratedDiagnosisPrefix
)

var (
	eventReceivedCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "event_received_count",
			Help: "Counter of events received by eventer",
		},
	)
	eventerDiagnosisGenerationSuccessCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "eventer_diagnosis_generation_success_count",
			Help: "Counter of successful diagnosis generations by eventer",
		},
	)
	eventerDiagnosisGenerationErrorCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "eventer_diagnosis_generation_error_count",
			Help: "Counter of erroneous diagnosis generations by eventer",
		},
	)
)

// Eventer generates diagnoses from kubernetes events.
type Eventer interface {
	// Run runs the Eventer.
	Run(<-chan struct{})
}

// eventer manages kubernetes events.
type eventer struct {
	// Context carries values across API boundaries.
	context.Context
	// Logger represents the ability to log messages.
	logr.Logger

	// client knows how to perform CRUD operations on Kubernetes objects.
	client client.Client
	// cache knows how to load Kubernetes objects.
	cache cache.Cache
	// nodeName specifies the node name.
	nodeName string
	// eventChainCh is a channel for queuing Events to be processed by eventer.
	eventChainCh chan corev1.Event
	// eventerEnabled indicates whether eventer is enabled.
	eventerEnabled bool
}

// NewEventer creates a new Eventer.
func NewEventer(
	ctx context.Context,
	logger logr.Logger,
	cli client.Client,
	cache cache.Cache,
	nodeName string,
	eventChainCh chan corev1.Event,
	eventerEnabled bool,
) Eventer {
	metrics.Registry.MustRegister(
		eventReceivedCount,
		eventerDiagnosisGenerationSuccessCount,
		eventerDiagnosisGenerationErrorCount,
	)

	return &eventer{
		Context:        ctx,
		Logger:         logger,
		eventChainCh:   eventChainCh,
		eventerEnabled: eventerEnabled,
	}
}

// Run runs the eventer.
func (ev *eventer) Run(stopCh <-chan struct{}) {
	if !ev.eventerEnabled {
		return
	}

	for {
		select {
		// Process events queuing in event channel.
		case event := <-ev.eventChainCh:
			eventReceivedCount.Inc()

			triggers, err := ev.listTriggers()
			if err != nil {
				ev.Error(err, "failed to list Triggers")
				return
			}

			diagnosis, err := ev.createDiagnosisFromKubernetesEvent(triggers, event)
			if err != nil {
				// Increment counter of erroneous diagnosis generations by eventer.
				eventerDiagnosisGenerationErrorCount.Inc()
				continue
			}

			ev.Info("creating Diagnosis from kubernetes event successfully", "diagnosis", client.ObjectKey{
				Name:      diagnosis.Name,
				Namespace: diagnosis.Namespace,
			})

			// Increment counter of successful diagnosis generations by eventer.
			eventerDiagnosisGenerationSuccessCount.Inc()
		// Stop source manager on stop signal.
		case <-stopCh:
			return
		}
	}
}

// listTriggers lists Triggers from cache.
func (ev *eventer) listTriggers() ([]diagnosisv1.Trigger, error) {
	var triggersList diagnosisv1.TriggerList
	if err := ev.cache.List(ev, &triggersList); err != nil {
		return nil, err
	}

	return triggersList.Items, nil
}

// createDiagnosisFromKubernetesEvent creates a Diagnosis from kubernetes event and triggers.
func (ev *eventer) createDiagnosisFromKubernetesEvent(triggers []diagnosisv1.Trigger, event corev1.Event) (*diagnosisv1.Diagnosis, error) {
	for _, trigger := range triggers {
		sourceTemplate := trigger.Spec.SourceTemplate
		if sourceTemplate.KubernetesEventTemplate != nil {
			// Set all fields of the diagnosis according to trigger if the kubernetes event contains
			// all match of the regular expression pattern defined in kubernetes event template.
			matched, err := matchKubernetesEvent(*sourceTemplate.KubernetesEventTemplate, event)
			if err != nil {
				ev.Error(err, "failed to compare trigger template and kubernetes event")
				continue
			}

			if matched {
				ev.Info("creating Diagnosis from kubernetes event", "event", client.ObjectKey{
					Name:      event.Name,
					Namespace: event.Namespace,
				})

				// Create diagnosis according to the kubernetes event.
				name := fmt.Sprintf("%s.%s.%s", KubernetesEventGeneratedDiagnosisPrefix, event.Namespace, event.Name)
				namespace := util.DefautlNamespace
				annotations := make(map[string]string)
				annotations[KubernetesEventAnnotation] = event.String()
				diagnosis := diagnosisv1.Diagnosis{
					ObjectMeta: metav1.ObjectMeta{
						Name:        name,
						Namespace:   namespace,
						Annotations: annotations,
					},
					Spec: diagnosisv1.DiagnosisSpec{
						OperationSet: trigger.Spec.OperationSet,
						NodeName:     event.Source.Host,
					},
				}

				if err := ev.client.Create(ev, &diagnosis); err != nil {
					if !apierrors.IsAlreadyExists(err) {
						ev.Error(err, "unable to create Diagnosis")
						return &diagnosis, err
					}
				}

				return &diagnosis, nil
			}
		}
	}

	return nil, nil
}

// matchKubernetesEvent reports whether the diagnosis contains all match of the regular expression pattern
// defined in kubernetes event template.
func matchKubernetesEvent(kubernetesEventTemplate diagnosisv1.KubernetesEventTemplate, event corev1.Event) (bool, error) {
	re, err := regexp.Compile(kubernetesEventTemplate.Regexp.Name)
	if err != nil {
		return false, err
	}
	if !re.MatchString(event.Name) {
		return false, nil
	}

	re, err = regexp.Compile(kubernetesEventTemplate.Regexp.Namespace)
	if err != nil {
		return false, err
	}
	if !re.MatchString(event.Namespace) {
		return false, nil
	}

	re, err = regexp.Compile(kubernetesEventTemplate.Regexp.Reason)
	if err != nil {
		return false, err
	}
	if !re.MatchString(event.Reason) {
		return false, nil
	}

	re, err = regexp.Compile(kubernetesEventTemplate.Regexp.Message)
	if err != nil {
		return false, err
	}
	if !re.MatchString(event.Message) {
		return false, nil
	}

	re, err = regexp.Compile(kubernetesEventTemplate.Regexp.Source.Component)
	if err != nil {
		return false, err
	}
	if !re.MatchString(event.Source.Component) {
		return false, nil
	}

	re, err = regexp.Compile(kubernetesEventTemplate.Regexp.Source.Host)
	if err != nil {
		return false, err
	}
	if !re.MatchString(event.Source.Host) {
		return false, nil
	}

	return true, nil
}