// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// These probes record current parser and resolver behavior for review.
// Their assertions do not define desired upgrade guarantees. Fake API reads
// bypass admission validation, watches, reconciliation, and upgrades.

package controllers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

func TestReleaseResolutionSuffixReproduction(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selected  string
		added     string
		wantAfter string
	}{
		{
			name:      "prerelease_suffix_moves",
			selected:  "1.34.4-custom.1",
			added:     "1.34.4-custom.2",
			wantAfter: "release-b",
		},
		{
			name:      "build_metadata_pin_stays",
			selected:  "1.34.4+custom.1",
			added:     "1.34.4+custom.2",
			wantAfter: "release-a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			scheme := runtime.NewScheme()
			require.NoError(t, internalv1.AddToScheme(scheme))
			makeRelease := func(name, kubernetesVersion string) *internalv1.ClusterRelease {
				return &internalv1.ClusterRelease{
					Name:              name,
					CreationTimestamp: metav1.NewTime(time.Unix(1, 0)),
					Annotations: map[string]string{
						internalv1.MachineInitKubernetesVersionAnnotation: kubernetesVersion,
					},
					Spec: internalv1.ClusterReleaseSpec{
						MachineImageRef: internalv1.MachineImageReference{Name: "test-image"},
						MachineInit: internalv1.MachineInit{
							UserDataTemplate: "#cloud-config\n",
						},
					},
				}
			}
			channel := &internalv1.ClusterReleaseChannel{
				Name: "test-channel",
				Spec: internalv1.ClusterReleaseChannelSpec{
					Releases: map[string]internalv1.ClusterReleaseReferences{
						tc.selected: {{Name: "release-a", MinimumUpgradeAge: metav1.Duration{Duration: 0}}},
					},
				},
			}
			apiClient := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(channel, makeRelease("release-a", "1.34.4")).Build()
			r := &NVKEClusterReconciler{Client: apiClient}
			cluster := &internalv1.NVKECluster{
				Name: "test-cluster", Namespace: "default", Generation: 1,
				Spec: internalv1.NVKEClusterSpec{
					ClusterNetwork: internalv1.ClusterNetwork{
						Pods:          internalv1.NetworkRanges{CIDRBlocks: []internalv1.NetworkCIDR{"10.244.0.0/16"}},
						Services:      internalv1.NetworkRanges{CIDRBlocks: []internalv1.NetworkCIDR{"10.96.0.0/16"}},
						ServiceDomain: "cluster.local",
					},
					Release: internalv1.ReleaseSelection{
						Channel: internalv1.ReleaseChannelReference{Name: "test-channel", Version: tc.selected},
					},
					ControlPlaneClass: "test-class",
				},
			}
			original := cluster.DeepCopy()
			initial, err := r.resolveClusterRelease(ctx, cluster)
			require.NoError(t, err)
			require.Equal(t, "release-a", initial.Name)
			t.Logf("new cluster: selector=%q channel=[%q] resolved=%s", tc.selected, tc.selected, initial.Name)

			// Channel suffixes are separate from the actual Kubernetes version.
			cluster.Status.ResolvedRelease = &internalv1.ReleaseReference{Name: "release-a", Version: "1.34.4"}
			require.NoError(t, apiClient.Create(ctx, makeRelease("release-b", "1.34.4")))
			require.NoError(t, apiClient.Get(ctx, client.ObjectKey{Name: channel.Name}, channel))
			channel.Spec.Releases[tc.added] = internalv1.ClusterReleaseReferences{
				{Name: "release-b", MinimumUpgradeAge: metav1.Duration{Duration: 0}},
			}
			require.NoError(t, apiClient.Update(ctx, channel))

			after, err := r.resolveClusterRelease(ctx, cluster)
			require.NoError(t, err)
			t.Logf("existing cluster: selector=%q status=release-a added=%q resolved=%s", tc.selected, tc.added, after.Name)
			require.Equal(t, tc.wantAfter, after.Name)
			require.Equal(t, original.Spec, cluster.Spec, "the cluster spec must stay unchanged")
			require.Equal(t, original.Generation, cluster.Generation)
		})
	}
}

const overflowReleaseVersion = "1.35.2.9999999999999999999999"
const overflowReleaseParseError = `illegal non-numeric version component "9999999999999999999999" in "1.35.2.9999999999999999999999": strconv.ParseUint: parsing "9999999999999999999999": value out of range`

func TestReleaseVersionOverflowParserReproduction(t *testing.T) {
	parsed, err := version.Parse(overflowReleaseVersion)
	t.Logf("version.Parse(%q): parsed=%v error=%v", overflowReleaseVersion, parsed, err)
	require.Nil(t, parsed)
	require.EqualError(t, err, overflowReleaseParseError)

	for _, input := range []string{"1.35.2.1234567", "1.35.2+9999999999999999999999"} {
		t.Run(input, func(t *testing.T) {
			parsed, err := version.Parse(input)
			require.NoError(t, err)
			t.Logf("version.Parse(%q): components=%v metadata=%q error=%v", input, parsed.Components(), parsed.BuildMetadata(), err)
		})
	}
}

func TestReleaseResolutionOverflowReproduction(t *testing.T) {
	for _, tc := range []struct {
		name               string
		selector           string
		includeChannel     bool
		includeOverflowKey bool
		wantError          string
	}{
		{
			name:           "exact_pin_with_valid_key_alone",
			selector:       "1.34.4.1",
			includeChannel: true,
		},
		{
			name:               "same_exact_pin_with_unrelated_overflowing_key",
			selector:           "1.34.4.1",
			includeChannel:     true,
			includeOverflowKey: true,
			wantError:          `parse channel "overflow-channel" version "1.35.2.9999999999999999999999": ` + overflowReleaseParseError,
		},
		{
			name:      "overflowing_selector_errors_before_missing_channel_lookup",
			selector:  overflowReleaseVersion,
			wantError: `parse requested channel version "1.35.2.9999999999999999999999": ` + overflowReleaseParseError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, internalv1.AddToScheme(scheme))
			objects := []client.Object{
				&internalv1.ClusterRelease{
					Name: "valid-release", Annotations: map[string]string{
						internalv1.MachineInitKubernetesVersionAnnotation: "1.34.4",
					},
					Spec: internalv1.ClusterReleaseSpec{
						MachineImageRef: internalv1.MachineImageReference{Name: "test-image"},
						MachineInit:     internalv1.MachineInit{UserDataTemplate: "#cloud-config\n"},
					},
				},
				&internalv1.ClusterRelease{
					Name: "overflow-release", Annotations: map[string]string{
						internalv1.MachineInitKubernetesVersionAnnotation: "1.35.2",
					},
					Spec: internalv1.ClusterReleaseSpec{
						MachineImageRef: internalv1.MachineImageReference{Name: "test-image"},
						MachineInit:     internalv1.MachineInit{UserDataTemplate: "#cloud-config\n"},
					},
				},
			}
			if tc.includeChannel {
				channel := &internalv1.ClusterReleaseChannel{
					Name: "overflow-channel",
					Spec: internalv1.ClusterReleaseChannelSpec{
						Releases: map[string]internalv1.ClusterReleaseReferences{
							"1.34.4.1": {{Name: "valid-release"}},
						},
					},
				}
				if tc.includeOverflowKey {
					channel.Spec.Releases[overflowReleaseVersion] = internalv1.ClusterReleaseReferences{{Name: "overflow-release"}}
				}
				objects = append(objects, channel)
			}
			r := &NVKEClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			cluster := &internalv1.NVKECluster{
				Spec: internalv1.NVKEClusterSpec{
					Release: internalv1.ReleaseSelection{
						Channel: internalv1.ReleaseChannelReference{Name: "overflow-channel", Version: tc.selector},
					},
				},
			}
			release, err := r.resolveClusterRelease(t.Context(), cluster)
			if tc.wantError != "" {
				t.Logf("resolveClusterRelease(selector=%q): release=%v error=%v", tc.selector, release, err)
				require.Nil(t, release)
				require.EqualError(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, release)
			t.Logf("resolveClusterRelease(selector=%q): release=%q error=%v", tc.selector, release.Name, err)
			require.Equal(t, "valid-release", release.Name)
		})
	}
}
