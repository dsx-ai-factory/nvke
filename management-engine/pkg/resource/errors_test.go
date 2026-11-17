// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// profile is a public shape with the field the unique-node-profile diagnostic names.
type profile struct {
	Spec struct {
		Tag string `json:"tag"`
	} `json:"spec"`
}

func TestTranslateAdmissionPolicyDenial(t *testing.T) {
	const policy = "unique-node-profile.service.nvke.nvidia.com"
	const denied = "ValidatingAdmissionPolicy '" + policy + "' with binding '" + policy + "' denied request: "
	const denial = denied + "node profile a:b is already published by NodeProfile c"
	approved := FieldError{
		Field: "/spec/tag", Code: invalidCode, Message: "Another node profile already publishes this profile and tag.",
	}
	for _, tc := range []struct {
		name  string
		cause metav1.StatusCause
		want  []FieldError
	}{
		{"denial", metav1.StatusCause{Message: denial}, []FieldError{approved}},
		{"unknown policy", metav1.StatusCause{Message: "ValidatingAdmissionPolicy 'other' denied request: x"}, nil},
		{"field cause", metav1.StatusCause{Field: "spec.profile", Message: denial}, nil},
		{"policy name in a value", metav1.StatusCause{Message: "Invalid value: \"" + policy + "\""}, nil},
		{"unterminated name", metav1.StatusCause{Message: "ValidatingAdmissionPolicy '" + policy}, nil},
		{"configuration failure", metav1.StatusCause{Message: denied + "failed to configure policy: " +
			"failed to find resource referenced by paramKind: 'service.nvke.nvidia.com/v1, Kind=NodeProfile'"}, nil},
		{"missing params", metav1.StatusCause{Message: denied +
			"no params found for policy binding with `Deny` parameterNotFoundAction"}, nil},
		{"evaluation error", metav1.StatusCause{Message: denied +
			"expression 'params.spec.tag != object.spec.tag' resulted in error: no such key: tag"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &apierrors.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure, Code: http.StatusUnprocessableEntity, Reason: metav1.StatusReasonInvalid,
				Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{tc.cause}},
			}}
			var public *Error
			require.True(t, errors.As(translate[profile](err), &public))
			require.Equal(t, http.StatusUnprocessableEntity, public.Status)
			require.Equal(t, invalidResourceCode, public.Code)
			require.Equal(t, tc.want, public.Errors)
		})
	}
	t.Run("field outside the public shape", func(t *testing.T) {
		err := &apierrors.StatusError{ErrStatus: metav1.Status{
			Reason: metav1.StatusReasonInvalid, Code: http.StatusUnprocessableEntity,
			Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Message: denial}}},
		}}
		var public *Error
		require.True(t, errors.As(translate[widget](err), &public))
		require.Empty(t, public.Errors)
	})
}
