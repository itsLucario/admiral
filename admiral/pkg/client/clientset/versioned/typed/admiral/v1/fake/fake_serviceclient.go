/*
Copyright The Kubernetes Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	admiralv1 "github.com/istio-ecosystem/admiral/admiral/pkg/apis/admiral/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeServiceClients implements ServiceClientInterface
type FakeServiceClients struct {
	Fake *FakeAdmiralV1
	ns   string
}

var serviceclientsResource = schema.GroupVersionResource{Group: "admiral.io", Version: "v1", Resource: "serviceclients"}

var serviceclientsKind = schema.GroupVersionKind{Group: "admiral.io", Version: "v1", Kind: "ServiceClient"}

// Get takes name of the serviceClient, and returns the corresponding serviceClient object, and an error if there is any.
func (c *FakeServiceClients) Get(name string, options v1.GetOptions) (result *admiralv1.ServiceClient, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(serviceclientsResource, c.ns, name), &admiralv1.ServiceClient{})

	if obj == nil {
		return nil, err
	}
	return obj.(*admiralv1.ServiceClient), err
}

// List takes label and field selectors, and returns the list of ServiceClients that match those selectors.
func (c *FakeServiceClients) List(opts v1.ListOptions) (result *admiralv1.ServiceClientList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(serviceclientsResource, serviceclientsKind, c.ns, opts), &admiralv1.ServiceClientList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &admiralv1.ServiceClientList{ListMeta: obj.(*admiralv1.ServiceClientList).ListMeta}
	for _, item := range obj.(*admiralv1.ServiceClientList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested serviceClients.
func (c *FakeServiceClients) Watch(opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(serviceclientsResource, c.ns, opts))

}

// Create takes the representation of a serviceClient and creates it.  Returns the server's representation of the serviceClient, and an error, if there is any.
func (c *FakeServiceClients) Create(serviceClient *admiralv1.ServiceClient) (result *admiralv1.ServiceClient, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(serviceclientsResource, c.ns, serviceClient), &admiralv1.ServiceClient{})

	if obj == nil {
		return nil, err
	}
	return obj.(*admiralv1.ServiceClient), err
}

// Update takes the representation of a serviceClient and updates it. Returns the server's representation of the serviceClient, and an error, if there is any.
func (c *FakeServiceClients) Update(serviceClient *admiralv1.ServiceClient) (result *admiralv1.ServiceClient, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(serviceclientsResource, c.ns, serviceClient), &admiralv1.ServiceClient{})

	if obj == nil {
		return nil, err
	}
	return obj.(*admiralv1.ServiceClient), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeServiceClients) UpdateStatus(serviceClient *admiralv1.ServiceClient) (*admiralv1.ServiceClient, error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceAction(serviceclientsResource, "status", c.ns, serviceClient), &admiralv1.ServiceClient{})

	if obj == nil {
		return nil, err
	}
	return obj.(*admiralv1.ServiceClient), err
}

// Delete takes name of the serviceClient and deletes it. Returns an error if one occurs.
func (c *FakeServiceClients) Delete(name string, options *v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteAction(serviceclientsResource, c.ns, name), &admiralv1.ServiceClient{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeServiceClients) DeleteCollection(options *v1.DeleteOptions, listOptions v1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(serviceclientsResource, c.ns, listOptions)

	_, err := c.Fake.Invokes(action, &admiralv1.ServiceClientList{})
	return err
}

// Patch applies the patch and returns the patched serviceClient.
func (c *FakeServiceClients) Patch(name string, pt types.PatchType, data []byte, subresources ...string) (result *admiralv1.ServiceClient, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(serviceclientsResource, c.ns, name, pt, data, subresources...), &admiralv1.ServiceClient{})

	if obj == nil {
		return nil, err
	}
	return obj.(*admiralv1.ServiceClient), err
}
