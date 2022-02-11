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

package services

import (
	"testing"

	"github.com/gravitational/teleport/api/types"

	"github.com/stretchr/testify/require"
)

// TestClusterConfigSchema verifies basic cluster config schema passes.
func TestClusterConfigSchema(t *testing.T) {
	cfg := DefaultClusterConfig()

	require.NoError(t, cfg.CheckAndSetDefaults())

	// default strategy is omitted during serialization, so explicitly set a non-zero value.
	cfg.SetRoutingStrategy(types.RoutingStrategy_MOST_RECENT)

	m, err := MarshalClusterConfig(cfg)
	require.NoError(t, err)

	_, err = UnmarshalClusterConfig(m)
	require.NoError(t, err)
}