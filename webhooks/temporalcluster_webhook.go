// Licensed to Alexandre VILAIN under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Alexandre VILAIN licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package webhooks

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/alexandrevilain/temporal-operator/api/v1beta1"
	"github.com/alexandrevilain/temporal-operator/pkg/discovery"
	"github.com/alexandrevilain/temporal-operator/pkg/version"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
)

// TemporalClusterWebhook provides endpoints to validate
// and set default fields values for TemporalCluster objects.
type TemporalClusterWebhook struct {
	AvailableAPIs *discovery.AvailableAPIs
}

func (w *TemporalClusterWebhook) getClusterFromRequest(obj runtime.Object) (*v1beta1.TemporalCluster, error) {
	cluster, ok := obj.(*v1beta1.TemporalCluster)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected an TemporalCluster but got a %T", obj))
	}
	return cluster, nil
}

func (w *TemporalClusterWebhook) aggregateClusterErrors(cluster *v1beta1.TemporalCluster, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		cluster.GroupVersionKind().GroupKind(),
		cluster.GetName(),
		errs,
	)
}

// Default ensures empty fields have their default value.
func (w *TemporalClusterWebhook) Default(ctx context.Context, obj runtime.Object) error {
	cluster, err := w.getClusterFromRequest(obj)
	if err != nil {
		return err
	}

	if cluster.Spec.Metrics.MetricsEnabled() {
		if cluster.Spec.Metrics.Prometheus != nil {
			// If the user has set the deprecated ListenAddress field and not the new ListenPort,
			// parse the listenAddress and set the listenPort.
			if cluster.Spec.Metrics.Prometheus.ListenAddress != "" && cluster.Spec.Metrics.Prometheus.ListenPort == nil {
				_, port, err := net.SplitHostPort(cluster.Spec.Metrics.Prometheus.ListenAddress)
				if err != nil {
					return fmt.Errorf("can't parse prometheus spec.metrics.prometheus.listenAddress: %w", err)
				}
				portInt, err := strconv.ParseInt(port, 10, 32)
				if err != nil {
					return fmt.Errorf("can't parse prometheus spec.metrics.prometheus.listenAddress port: %w", err)
				}
				cluster.Spec.Metrics.Prometheus.ListenAddress = "" // Empty the listen adress
				cluster.Spec.Metrics.Prometheus.ListenPort = pointer.Int32(int32(portInt))
			}
		}
	}

	// Finish by setting default values
	cluster.Default()

	return nil
}

func (w *TemporalClusterWebhook) validateCluster(cluster *v1beta1.TemporalCluster) field.ErrorList {
	var errs field.ErrorList

	// If mTLS is enabled using cert-manager, but cert-manager support is disabled on the controller
	// it can't process the request, return the error.
	if cluster.MTLSWithCertManagerEnabled() && !w.AvailableAPIs.CertManager {
		errs = append(errs,
			field.Invalid(
				field.NewPath("spec", "mTLS", "provider"),
				cluster.Spec.MTLS.Provider,
				"Can't use cert-manager as mTLS provider as it's not available in the cluster",
			),
		)
	}

	// Validate that the cluster version is a supported one.
	err := cluster.Spec.Version.Validate()
	if err != nil {
		errs = append(errs,
			field.Forbidden(
				field.NewPath("spec", "version"),
				fmt.Sprintf("Unsupported temporal version (supported: %s)", version.SupportedVersionsRange.String()),
			),
		)
	}

	// Ensure ElasticSearch v6 is not used with cluster >= 1.18.0
	if cluster.Spec.Version.GreaterOrEqual(version.V1_18_0) &&
		cluster.Spec.Persistence.AdvancedVisibilityStore != nil &&
		cluster.Spec.Persistence.AdvancedVisibilityStore.Elasticsearch != nil &&
		cluster.Spec.Persistence.AdvancedVisibilityStore.Elasticsearch.Version == "v6" {

		errs = append(errs,
			field.Forbidden(
				field.NewPath("spec", "persistence", "advancedVisibilityStore", "elasticsearch", "version"),
				"temporal cluster version >= 1.18.0 doesn't support ElasticSearch v6",
			),
		)
	}

	return errs
}

// ValidateCreate ensures the user is creating a consistent temporal cluster.
func (w *TemporalClusterWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) error {
	cluster, err := w.getClusterFromRequest(obj)
	if err != nil {
		return err
	}

	errs := w.validateCluster(cluster)

	return w.aggregateClusterErrors(cluster, errs)
}

// ValidateUpdate validates TemporalCluster updates.
// It mainly check for sequential version upgrades.
func (w *TemporalClusterWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) error {
	oldCluster, err := w.getClusterFromRequest(oldObj)
	if err != nil {
		return err
	}

	newCluster, err := w.getClusterFromRequest(newObj)
	if err != nil {
		return err
	}

	errs := w.validateCluster(newCluster)

	// Ensure user is doing a sequential version upgrade.
	// See: https://docs.temporal.io/cluster-deployment-guide#upgrade-server
	constraint, err := oldCluster.Spec.Version.UpgradeConstraint()
	if err != nil {
		return fmt.Errorf("can't compute version upgrade constraint: %w", err)
	}

	allowed := constraint.Check(newCluster.Spec.Version.Version)
	if !allowed {
		errs = append(errs,
			field.Forbidden(
				field.NewPath("spec", "version"),
				"Unauthorized version upgrade. Only sequential version upgrades are allowed (from v1.n.x to v1.n+1.x)",
			),
		)
	}

	return w.aggregateClusterErrors(newCluster, errs)
}

// ValidateDelete does nothing.
func (w *TemporalClusterWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) error {
	// No delete validation needed.
	return nil
}

// +kubebuilder:webhook:path=/validate-temporal-io-v1beta1-temporalcluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=temporal.io,resources=temporalclusters,verbs=create;update,versions=v1beta1,name=vtemporalc.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/mutate-temporal-io-v1beta1-temporalcluster,mutating=true,failurePolicy=fail,sideEffects=None,groups=temporal.io,resources=temporalclusters,verbs=create;update,versions=v1beta1,name=mtemporalc.kb.io,admissionReviewVersions=v1

func (w *TemporalClusterWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&v1beta1.TemporalCluster{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}
