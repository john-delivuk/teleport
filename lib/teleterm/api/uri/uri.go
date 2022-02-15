/*
Copyright 2021 Gravitational, Inc.

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

package uri

import (
	"fmt"

	"github.com/gravitational/trace"
)

var pathClusters = NewPath("/clusters/:cluster/*")
var pathLeafClusters = NewPath("/clusters/:cluster/leaves/:leaf/*")

// New creates an instance of ResourceURI
func New(path string) ResourceURI {
	return ResourceURI{
		Path: path,
	}
}

// NewClusterURI creates a new cluster URI for given cluster name
func NewClusterURI(clusterName string) ResourceURI {
	return ResourceURI{
		Path: fmt.Sprintf("/clusters/%v", clusterName),
	}
}

// ParseClusterURI parses a string and returns cluster URI
func ParseClusterURI(path string) (ResourceURI, error) {
	URI := New(path)
	rootClusterName := URI.GetRootClusterName()
	leafClusterName := URI.GetLeafClusterName()

	if rootClusterName == "" {
		return URI, trace.BadParameter("missing root cluster name")
	}

	clusterURI := NewClusterURI(rootClusterName)
	if leafClusterName != "" {
		clusterURI = clusterURI.AppendLeafCluster(leafClusterName)
	}

	return clusterURI, nil
}

// NewGatewayURI creates a gateway URI for a given ID
func NewGatewayURI(id string) ResourceURI {
	return ResourceURI{
		Path: fmt.Sprintf("/gateways/%v", id),
	}
}

// ResourceURI describes resource URI
type ResourceURI struct {
	Path string
}

// GetRootClusterName returns root cluster name
func (r ResourceURI) GetRootClusterName() string {
	result, ok := pathClusters.Match(r.Path + "/")
	if !ok {
		return ""
	}

	return result.Params["cluster"]
}

// GetLeafClusterName returns leaf cluster name
func (r ResourceURI) GetLeafClusterName() string {
	result, ok := pathLeafClusters.Match(r.Path + "/")
	if !ok {
		return ""
	}

	return result.Params["leaf"]
}

// AppendServer appends server segment to the URI
func (r ResourceURI) AppendServer(id string) ResourceURI {
	r.Path = fmt.Sprintf("%v/servers/%v", r.Path, id)
	return r
}

// AppendLeafCluster appends leaf cluster segment to the URI
func (r ResourceURI) AppendLeafCluster(name string) ResourceURI {
	r.Path = fmt.Sprintf("%v/leaves/%v", r.Path, name)
	return r
}

// AppendKube appends kube segment to the URI
func (r ResourceURI) AppendKube(name string) ResourceURI {
	r.Path = fmt.Sprintf("%v/kubes/%v", r.Path, name)
	return r
}

// AppendDB appends database segment to the URI
func (r ResourceURI) AppendDB(name string) ResourceURI {
	r.Path = fmt.Sprintf("%v/dbs/%v", r.Path, name)
	return r
}

// AddGateway appends gateway segment to the URI
func (r ResourceURI) AddGateway(id string) ResourceURI {
	r.Path = fmt.Sprintf("%v/gateways/%v", r.Path, id)
	return r
}

// AppendApp appends app segment to the URI
func (r ResourceURI) AppendApp(name string) ResourceURI {
	r.Path = fmt.Sprintf("%v/apps/%v", r.Path, name)
	return r
}

// String returns string represantion of the Resource URI
func (r ResourceURI) String() string {
	return r.Path
}
