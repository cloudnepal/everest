// everest
// Copyright (C) 2023 Percona LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/AlekSi/pointer"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	everestv1alpha1 "github.com/percona/everest-operator/api/v1alpha1"
	"github.com/percona/everest/pkg/pmm"
	"github.com/percona/everest/pkg/rbac"
)

const (
	// MonitoringNamespace is the namespace where monitoring configs are created.
	MonitoringNamespace = "everest-monitoring"
)

// CreateMonitoringInstance creates a new monitoring instance.
func (e *EverestServer) CreateMonitoringInstance(ctx echo.Context, namespace string) error {
	params, err := validateCreateMonitoringInstanceRequest(ctx)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, Error{Message: pointer.ToString(err.Error())})
	}
	c := ctx.Request().Context()
	m, err := e.kubeClient.GetMonitoringConfig(c, namespace, params.Name)
	if err != nil && !k8serrors.IsNotFound(err) {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Could not get monitoring instance"),
		})
	}
	// TODO: Change the design of operator's structs so they return nil struct so
	// if s != nil passes
	if m != nil && m.Name != "" {
		err = fmt.Errorf("monitoring instance %s already exists in namespace %s", params.Name, namespace)
		e.l.Error(err)
		return ctx.JSON(http.StatusConflict, Error{Message: pointer.ToString(err.Error())})
	}

	apiKey, err := e.getPMMApiKey(c, params)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Could not create an API key in PMM"),
		})
	}

	if err := e.createMonitoringK8sResources(c, namespace, params, apiKey); err != nil {
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString(err.Error()),
		})
	}

	result := MonitoringInstance{
		Type:              MonitoringInstanceBaseWithNameType(params.Type),
		Name:              params.Name,
		Namespace:         namespace,
		Url:               params.Url,
		AllowedNamespaces: params.AllowedNamespaces,
		VerifyTLS:         params.VerifyTLS,
	}

	return ctx.JSON(http.StatusOK, result)
}

func (e *EverestServer) getPMMApiKey(ctx context.Context, params *CreateMonitoringInstanceJSONRequestBody) (string, error) {
	if params.Pmm != nil && params.Pmm.ApiKey != "" {
		return params.Pmm.ApiKey, nil
	}

	e.l.Debug("Getting PMM API key by username and password")
	skipVerifyTLS := !pointer.Get(params.VerifyTLS)
	return pmm.CreatePMMApiKey(
		ctx, params.Url, fmt.Sprintf("everest-%s-%s", params.Name, uuid.NewString()),
		params.Pmm.User, params.Pmm.Password,
		skipVerifyTLS,
	)
}

func (e *EverestServer) createMonitoringK8sResources(
	c context.Context, namespace string, params *CreateMonitoringInstanceJSONRequestBody, apiKey string,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      params.Name,
			Namespace: namespace,
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: e.monitoringConfigSecretData(apiKey),
	}
	if _, err := e.kubeClient.CreateSecret(c, secret); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			_, err = e.kubeClient.UpdateSecret(c, secret)
			if err != nil {
				e.l.Error(err)
				return fmt.Errorf("could not update k8s secret %s", params.Name)
			}
		} else {
			e.l.Error(err)
			return fmt.Errorf("failed creating secret in the Kubernetes cluster")
		}
	}
	err := e.kubeClient.CreateMonitoringConfig(c, &everestv1alpha1.MonitoringConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      params.Name,
			Namespace: namespace,
		},
		Spec: everestv1alpha1.MonitoringConfigSpec{
			Type: everestv1alpha1.MonitoringType(params.Type),
			PMM: everestv1alpha1.PMMConfig{
				URL: params.Url,
			},
			CredentialsSecretName: params.Name,
			VerifyTLS:             params.VerifyTLS,
		},
	})
	if err != nil {
		e.l.Error(err)
		if dErr := e.kubeClient.DeleteSecret(c, namespace, params.Name); dErr != nil {
			return fmt.Errorf("failed cleaning up the secret because failed creating monitoring instance")
		}
		return fmt.Errorf("failed creating monitoring instance")
	}

	return nil
}

// enforceMonitoringConfigRBAC checks if the user has permissions to read the monitoring config.
func (e *EverestServer) enforceMonitoringConfigRBAC(user string, mc everestv1alpha1.MonitoringConfig) error {
	// Check if the user has permissions for this monitoring config.
	if err := e.enforce(user, rbac.ResourceMonitoringInstances, rbac.ActionRead, rbac.ObjectName(mc.GetNamespace(), mc.GetName())); err != nil {
		if !errors.Is(err, errInsufficientPermissions) {
			e.l.Error(errors.Join(err, errors.New("failed to check monitoring-instance permissions")))
		}
		return err
	}

	return nil
}

// ListMonitoringInstances lists all monitoring instances.
func (e *EverestServer) ListMonitoringInstances(ctx echo.Context, namespace string) error {
	user, err := rbac.GetUser(ctx)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed to get user from context" + err.Error()),
		})
	}

	mcList, err := e.kubeClient.ListMonitoringConfigs(ctx.Request().Context(), namespace)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{Message: pointer.ToString("Could not get a list of monitoring instances")})
	}

	result := make([]*MonitoringInstance, 0, len(mcList.Items))
	for _, mc := range mcList.Items {
		if err := e.enforceMonitoringConfigRBAC(user, mc); errors.Is(err, errInsufficientPermissions) {
			continue
		} else if err != nil {
			return err
		}

		result = append(result, &MonitoringInstance{
			Type:      MonitoringInstanceBaseWithNameType(mc.Spec.Type),
			Name:      mc.GetName(),
			Namespace: mc.GetNamespace(),
			Url:       mc.Spec.PMM.URL,
			//nolint:exportloopref
			AllowedNamespaces: &mc.Spec.AllowedNamespaces,
			VerifyTLS:         mc.Spec.VerifyTLS,
		})
	}
	return ctx.JSON(http.StatusOK, result)
}

// GetMonitoringInstance retrieves a monitoring instance.
func (e *EverestServer) GetMonitoringInstance(ctx echo.Context, namespace, name string) error {
	m, err := e.kubeClient.GetMonitoringConfig(ctx.Request().Context(), namespace, name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return ctx.JSON(http.StatusNotFound, Error{
				Message: pointer.ToString("Monitoring instance is not found"),
			})
		}
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{Message: pointer.ToString("Could not get a list of monitoring instances")})
	}

	return ctx.JSON(http.StatusOK, &MonitoringInstance{
		Type:              MonitoringInstanceBaseWithNameType(m.Spec.Type),
		Name:              m.GetName(),
		Namespace:         m.GetNamespace(),
		Url:               m.Spec.PMM.URL,
		AllowedNamespaces: &m.Spec.AllowedNamespaces,
		VerifyTLS:         m.Spec.VerifyTLS,
	})
}

// UpdateMonitoringInstance updates a monitoring instance based on the provided fields.
func (e *EverestServer) UpdateMonitoringInstance(ctx echo.Context, namespace, name string) error { //nolint:funlen,cyclop
	c := ctx.Request().Context()
	m, err := e.kubeClient.GetMonitoringConfig(c, namespace, name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return ctx.JSON(http.StatusNotFound, Error{
				Message: pointer.ToString("Monitoring instance is not found"),
			})
		}
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed getting monitoring instance"),
		})
	}

	params, err := e.validateUpdateMonitoringInstanceRequest(ctx)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, Error{Message: pointer.ToString(err.Error())})
	}

	var apiKey string
	if params.Pmm != nil && params.Pmm.ApiKey != "" {
		apiKey = params.Pmm.ApiKey
	}
	skipVerifyTLS := !pointer.Get(params.VerifyTLS)
	if params.Pmm != nil && params.Pmm.User != "" && params.Pmm.Password != "" {
		apiKey, err = pmm.CreatePMMApiKey(
			c, params.Url, fmt.Sprintf("everest-%s-%s", name, uuid.NewString()),
			params.Pmm.User, params.Pmm.Password,
			skipVerifyTLS,
		)
		if err != nil {
			e.l.Error(err)
			return ctx.JSON(http.StatusInternalServerError, Error{
				Message: pointer.ToString("Could not create an API key in PMM"),
			})
		}
	}
	if apiKey != "" {
		_, err = e.kubeClient.UpdateSecret(c, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Type:       corev1.SecretTypeOpaque,
			StringData: e.monitoringConfigSecretData(apiKey),
		})
		if err != nil {
			e.l.Error(err)
			return ctx.JSON(http.StatusInternalServerError, Error{
				Message: pointer.ToString(fmt.Sprintf("Could not update k8s secret %s", name)),
			})
		}
	}
	if params.Url != "" {
		m.Spec.PMM.URL = params.Url
	}
	if params.AllowedNamespaces != nil {
		m.Spec.AllowedNamespaces = *params.AllowedNamespaces
	}
	if params.VerifyTLS != nil {
		m.Spec.VerifyTLS = params.VerifyTLS
	}
	err = e.kubeClient.UpdateMonitoringConfig(c, m)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed updating monitoring instance"),
		})
	}

	return ctx.JSON(http.StatusOK, &MonitoringInstance{
		Type:              MonitoringInstanceBaseWithNameType(m.Spec.Type),
		Name:              m.GetName(),
		Namespace:         m.GetNamespace(),
		Url:               m.Spec.PMM.URL,
		AllowedNamespaces: &m.Spec.AllowedNamespaces,
		VerifyTLS:         m.Spec.VerifyTLS,
	})
}

// DeleteMonitoringInstance deletes a monitoring instance.
func (e *EverestServer) DeleteMonitoringInstance(ctx echo.Context, namespace, name string) error {
	used, err := e.kubeClient.IsMonitoringConfigUsed(ctx.Request().Context(), namespace, name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return ctx.JSON(http.StatusNotFound, Error{
				Message: pointer.ToString("Monitoring instance is not found"),
			})
		}
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed to check the monitoring instance is used"),
		})
	}
	if used {
		return ctx.JSON(http.StatusBadRequest, Error{
			Message: pointer.ToString(fmt.Sprintf("Monitoring instance %s is used", name)),
		})
	}
	if err := e.kubeClient.DeleteMonitoringConfig(ctx.Request().Context(), namespace, name); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctx.JSON(http.StatusNotFound, Error{
				Message: pointer.ToString("Monitoring instance is not found"),
			})
		}
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed to get monitoring instance"),
		})
	}
	if err := e.kubeClient.DeleteSecret(ctx.Request().Context(), namespace, name); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctx.NoContent(http.StatusNoContent)
		}
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Failed deleting monitoring instance"),
		})
	}
	return ctx.NoContent(http.StatusNoContent)
}

func (e *EverestServer) monitoringConfigSecretData(apiKey string) map[string]string {
	return map[string]string{
		"apiKey":   apiKey,
		"username": "api_key",
	}
}
