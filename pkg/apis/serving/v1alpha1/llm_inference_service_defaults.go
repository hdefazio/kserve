/*

Copyright 2025 The KServe Authors.

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

package v1alpha1

import (
	"context"

	"k8s.io/utils/ptr"
	"knative.dev/pkg/apis"
)

var _ apis.Defaultable = &LLMInferenceService{}

func (in *LLMInferenceService) SetDefaults(_ context.Context) {
	if in.Spec.Model.Name == nil || *in.Spec.Model.Name == "" {
		in.Spec.Model.Name = ptr.To(in.GetName())
	}
}
