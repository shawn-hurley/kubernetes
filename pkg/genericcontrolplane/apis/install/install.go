/*
Copyright 2020 The Kubernetes Authors.

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

package install

import (
	"k8s.io/kubernetes/pkg/api/genericcontrolplanescheme"
	authenticationinstall "k8s.io/kubernetes/pkg/apis/authentication/install"
	authorizationinstall "k8s.io/kubernetes/pkg/apis/authorization/install"
	certificatesinstall "k8s.io/kubernetes/pkg/apis/certificates/install"
	coordinationinstall "k8s.io/kubernetes/pkg/apis/coordination/install"
	genericcontrolplaneinstall "k8s.io/kubernetes/pkg/apis/core/install/genericcontrolplane"
	eventsinstall "k8s.io/kubernetes/pkg/apis/events/install"
	flowcontrolinstall "k8s.io/kubernetes/pkg/apis/flowcontrol/install"
	rbacinstall "k8s.io/kubernetes/pkg/apis/rbac/install"
)

func init() {
	genericcontrolplaneinstall.Install(genericcontrolplanescheme.Scheme)
	authenticationinstall.Install(genericcontrolplanescheme.Scheme)
	authorizationinstall.Install(genericcontrolplanescheme.Scheme)
	certificatesinstall.Install(genericcontrolplanescheme.Scheme)
	coordinationinstall.Install(genericcontrolplanescheme.Scheme)
	rbacinstall.Install(genericcontrolplanescheme.Scheme)
	flowcontrolinstall.Install(genericcontrolplanescheme.Scheme)
	eventsinstall.Install(genericcontrolplanescheme.Scheme)
}
