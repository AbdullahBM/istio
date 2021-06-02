// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/gogo/protobuf/types"

	"istio.io/api/annotation"
	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/util/network"
	"istio.io/istio/pkg/bootstrap"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/validation"
	"istio.io/pkg/log"
)

// return proxyConfig and trustDomain
func ConstructProxyConfig(meshConfigFile, serviceCluster, proxyConfigEnv string, concurrency int, role *model.Proxy) (*meshconfig.ProxyConfig, error) {
	// annotation 注解
	annotations, err := readPodAnnotations()
	if err != nil {
		if os.IsNotExist(err) {
			log.Debugf("failed to read pod annotations: %v", err)
		} else {
			log.Warnf("failed to read pod annotations: %v", err)
		}
	}

	// mesh configfile 文件
	var fileMeshContents string
	if fileExists(meshConfigFile) {
		contents, err := ioutil.ReadFile(meshConfigFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read mesh config file %v: %v", meshConfigFile, err)
		}
		fileMeshContents = string(contents)
	}

	// meshconfig 配置文件
	meshConfig, err := getMeshConfig(fileMeshContents, annotations[annotation.ProxyConfig.Name], proxyConfigEnv)
	if err != nil {
		return nil, err
	}
	proxyConfig := mesh.DefaultProxyConfig()
	if meshConfig.DefaultConfig != nil {
		proxyConfig = *meshConfig.DefaultConfig
	}

	// If concurrency is unset, we will automatically set this based on CPU requests/limits for sidecars.
	// For gateways, this will use all available CPUs.
	// If explicitly set concurrency flag, this will be used.
	// 设置 concurrency 值
	if concurrency == 0 && role.Type == model.SidecarProxy {
		byResources := determineConcurrencyOption()
		if byResources != nil {
			proxyConfig.Concurrency = byResources
		}
	} else {
		proxyConfig.Concurrency = &types.Int32Value{Value: int32(concurrency)}
	}

	// 服务的名称
	proxyConfig.ServiceCluster = serviceCluster
	// resolve statsd address
	if proxyConfig.StatsdUdpAddress != "" {
		// ResolveAddr 将授权地址解析为 IP 地址。传入地址可以是 IP 地址或主机名。如果 addr 是 IPv6 地址，则 IP 部分必须用方括号括起来。
		addr, err := network.ResolveAddr(proxyConfig.StatsdUdpAddress)
		if err != nil {
			log.Warnf("resolve StatsdUdpAddress failed: %v", err)
			proxyConfig.StatsdUdpAddress = ""
		} else {
			proxyConfig.StatsdUdpAddress = addr
		}
	}
	if err := validation.ValidateProxyConfig(&proxyConfig); err != nil {
		return nil, err
	}
	// 配置 pod annotation 上的注解
	return applyAnnotations(&proxyConfig, annotations), nil
}

// determineConcurrencyOption determines the correct setting for --concurrency based on CPU requests/limits
func determineConcurrencyOption() *types.Int32Value {
	// If limit is set, us that
	// The format in the file is a plain integer. `100` in the file is equal to `100m` (based on `divisor: 1m`
	// in the pod spec).
	// With the resource setting, we round up to single integer number; for example, if we have a 500m limit
	// the pod will get concurrency=1. With 6500m, it will get concurrency=7.
	limit, err := readPodCPULimits()
	if err == nil && limit > 0 {
		return &types.Int32Value{Value: int32(math.Ceil(float64(limit) / 1000))}
	}
	// If limit is unset, use requests instead, with the same logic.
	requests, err := readPodCPURequests()
	if err == nil && requests > 0 {
		return &types.Int32Value{Value: int32(math.Ceil(float64(requests) / 1000))}
	}
	return nil
}

// getMeshConfig gets the mesh config to use for proxy configuration
// 1. First we take the default config
// 2. Then we apply any settings from file (this comes from gateway mounting configmap)
// 3. Then we apply settings from environment variable (this comes from sidecar injection sticking meshconfig here)
// 4. Then we apply overrides from annotation (this comes from annotation on gateway, passed through downward API)
//
// Merging is done by replacement. Any fields present in the overlay will replace those existing fields, while
// untouched fields will remain untouched. This means lists will be replaced, not appended to, for example.
func getMeshConfig(fileOverride, annotationOverride, proxyConfigEnv string) (meshconfig.MeshConfig, error) {
	mc := mesh.DefaultMeshConfig()

	if fileOverride != "" {
		log.Infof("Apply mesh config from file %v", fileOverride)
		fileMesh, err := mesh.ApplyMeshConfig(fileOverride, mc)
		if err != nil || fileMesh == nil {
			return meshconfig.MeshConfig{}, fmt.Errorf("failed to unmarshal mesh config from file [%v]: %v", fileOverride, err)
		}
		mc = *fileMesh
	}

	// 2021-06-01T03:00:04.994441Z	info	Apply proxy config from env
	/**
	{
	    "discoveryAddress":"istiod-iop-1-8-4.istio-system.svc:15012",
	    "tracing":{
	        "zipkin":{
	            "address":"jaeger-prod-elasticsearch-collector.mesh:9411"
	        },
	        "customTags":{
	            "mesh":{
	                "header":{
	                    "name":"mesh",
	                    "defaultValue":"mesh"
	                }
	            },
	            "tag_clustername":{
	                "literal":{
	                    "value":"axzq-test"
	                }
	            }
	        },
	        "sampling":100
	    },
	    "proxyMetadata":{
	        "DNS_AGENT":""
	    }
	}
	*/
	if proxyConfigEnv != "" {
		log.Infof("Apply proxy config from env %v", proxyConfigEnv)
		envMesh, err := mesh.ApplyProxyConfig(proxyConfigEnv, mc)
		if err != nil || envMesh == nil {
			return meshconfig.MeshConfig{}, fmt.Errorf("failed to unmarshal mesh config from environment [%v]: %v", proxyConfigEnv, err)
		}
		mc = *envMesh
	}

	// 注解覆盖
	if annotationOverride != "" {
		log.Infof("Apply proxy config from annotation %v", annotationOverride)
		annotationMesh, err := mesh.ApplyProxyConfig(annotationOverride, mc)
		if err != nil || annotationMesh == nil {
			return meshconfig.MeshConfig{}, fmt.Errorf("failed to unmarshal mesh config from annotation [%v]: %v", annotationOverride, err)
		}
		mc = *annotationMesh
	}

	return mc, nil
}

func fileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func readPodAnnotations() (map[string]string, error) {
	b, err := ioutil.ReadFile(constants.PodInfoAnnotationsPath)
	if err != nil {
		return nil, err
	}
	return bootstrap.ParseDownwardAPI(string(b))
}

func readPodCPURequests() (int, error) {
	b, err := ioutil.ReadFile(constants.PodInfoCPURequestsPath)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(b))
}

func readPodCPULimits() (int, error) {
	b, err := ioutil.ReadFile(constants.PodInfoCPULimitsPath)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(b))
}

// Apply any overrides to proxy config from annotations
func applyAnnotations(config *meshconfig.ProxyConfig, annos map[string]string) *meshconfig.ProxyConfig {
	if v, f := annos[annotation.SidecarDiscoveryAddress.Name]; f {
		config.DiscoveryAddress = v
	}
	if v, f := annos[annotation.SidecarStatusPort.Name]; f {
		p, err := strconv.Atoi(v)
		if err != nil {
			log.Errorf("Invalid annotation %v=%v: %v", annotation.SidecarStatusPort, p, err)
		}
		// 代理应在其上侦听管理命令（例如就绪探测）的端口。
		config.StatusPort = int32(p)
	}
	return config
}

func GetPilotSan(discoveryAddress string) string {
	discHost := strings.Split(discoveryAddress, ":")[0]
	// For local debugging - the discoveryAddress is set to localhost, but the cert issued for normal SA.
	if discHost == "localhost" {
		discHost = "istiod.istio-system.svc"
	}
	return discHost
}
