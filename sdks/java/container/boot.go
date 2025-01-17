// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// boot is the boot code for the Java SDK harness container. It is responsible
// for retrieving staged files and invoking the JVM correctly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/apache/beam/sdks/v2/go/pkg/beam/artifact"
	fnpb "github.com/apache/beam/sdks/v2/go/pkg/beam/model/fnexecution_v1"
	pipepb "github.com/apache/beam/sdks/v2/go/pkg/beam/model/pipeline_v1"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/provision"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/util/execx"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/util/grpcx"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/util/syscallx"
	"github.com/golang/protobuf/proto"
)

var (
	// Contract: https://s.apache.org/beam-fn-api-container-contract.

	id                = flag.String("id", "", "Local identifier (required).")
	loggingEndpoint   = flag.String("logging_endpoint", "", "Logging endpoint (required).")
	artifactEndpoint  = flag.String("artifact_endpoint", "", "Artifact endpoint (required).")
	provisionEndpoint = flag.String("provision_endpoint", "", "Provision endpoint (required).")
	controlEndpoint   = flag.String("control_endpoint", "", "Control endpoint (required).")
	semiPersistDir    = flag.String("semi_persist_dir", "/tmp", "Local semi-persistent directory (optional).")
)

const (
	disableJammAgentOption              = "disable_jamm_agent"
	enableGoogleCloudProfilerOption     = "enable_google_cloud_profiler"
	enableGoogleCloudHeapSamplingOption = "enable_google_cloud_heap_sampling"
	googleCloudProfilerAgentBaseArgs    = "-agentpath:/opt/google_cloud_profiler/profiler_java_agent.so=-logtostderr,-cprof_service=%s,-cprof_service_version=%s"
	googleCloudProfilerAgentHeapArgs    = googleCloudProfilerAgentBaseArgs + ",-cprof_enable_heap_sampling,-cprof_heap_sampling_interval=2097152"
	jammAgentArgs                       = "-javaagent:/opt/apache/beam/jars/jamm.jar"
)

func main() {
	flag.Parse()
	if *id == "" {
		log.Fatal("No id provided.")
	}
	if *provisionEndpoint == "" {
		log.Fatal("No provision endpoint provided.")
	}

	ctx := grpcx.WriteWorkerID(context.Background(), *id)

	info, err := provision.Info(ctx, *provisionEndpoint)
	if err != nil {
		log.Fatalf("Failed to obtain provisioning information: %v", err)
	}
	log.Printf("Provision info:\n%v", info)

	// TODO(BEAM-8201): Simplify once flags are no longer used.
	if info.GetLoggingEndpoint().GetUrl() != "" {
		*loggingEndpoint = info.GetLoggingEndpoint().GetUrl()
	}
	if info.GetArtifactEndpoint().GetUrl() != "" {
		*artifactEndpoint = info.GetArtifactEndpoint().GetUrl()
	}
	if info.GetControlEndpoint().GetUrl() != "" {
		*controlEndpoint = info.GetControlEndpoint().GetUrl()
	}

	if *loggingEndpoint == "" {
		log.Fatal("No logging endpoint provided.")
	}
	if *artifactEndpoint == "" {
		log.Fatal("No artifact endpoint provided.")
	}
	if *controlEndpoint == "" {
		log.Fatal("No control endpoint provided.")
	}

	log.Printf("Initializing java harness: %v", strings.Join(os.Args, " "))

	// (1) Obtain the pipeline options

	options, err := provision.ProtoToJSON(info.GetPipelineOptions())
	if err != nil {
		log.Fatalf("Failed to convert pipeline options: %v", err)
	}

	// (2) Retrieve the staged user jars. We ignore any disk limit,
	// because the staged jars are mandatory.

	// Using the SDK Harness ID in the artifact destination path to make sure that dependencies used by multiple
	// SDK Harnesses in the same VM do not conflict. This is needed since some runners (for example, Dataflow)
	// may share the artifact staging directory across multiple SDK Harnesses
	// TODO(BEAM-9455): consider removing the SDK Harness ID from the staging path after Dataflow can properly
	// seperate out dependencies per environment.
	dir := filepath.Join(*semiPersistDir, *id, "staged")

	artifacts, err := artifact.Materialize(ctx, *artifactEndpoint, info.GetDependencies(), info.GetRetrievalToken(), dir)
	if err != nil {
		log.Fatalf("Failed to retrieve staged files: %v", err)
	}

	// (3) Invoke the Java harness, preserving artifact ordering in classpath.

	os.Setenv("HARNESS_ID", *id)
	os.Setenv("PIPELINE_OPTIONS", options)
	os.Setenv("LOGGING_API_SERVICE_DESCRIPTOR", proto.MarshalTextString(&pipepb.ApiServiceDescriptor{Url: *loggingEndpoint}))
	os.Setenv("CONTROL_API_SERVICE_DESCRIPTOR", proto.MarshalTextString(&pipepb.ApiServiceDescriptor{Url: *controlEndpoint}))
	os.Setenv("RUNNER_CAPABILITIES", strings.Join(info.GetRunnerCapabilities(), " "))

	if info.GetStatusEndpoint() != nil {
		os.Setenv("STATUS_API_SERVICE_DESCRIPTOR", proto.MarshalTextString(info.GetStatusEndpoint()))
	}

	const jarsDir = "/opt/apache/beam/jars"
	cp := []string{
		filepath.Join(jarsDir, "slf4j-api.jar"),
		filepath.Join(jarsDir, "slf4j-jdk14.jar"),
		filepath.Join(jarsDir, "beam-sdks-java-harness.jar"),
		filepath.Join(jarsDir, "beam-sdks-java-io-kafka.jar"),
		filepath.Join(jarsDir, "kafka-clients.jar"),
	}

	var hasWorkerExperiment = strings.Contains(options, "use_staged_dataflow_worker_jar")
	for _, a := range artifacts {
		name, _ := artifact.MustExtractFilePayload(a)
		if hasWorkerExperiment {
			if strings.HasPrefix(name, "beam-runners-google-cloud-dataflow-java-fn-api-worker") {
				continue
			}
			if name == "dataflow-worker.jar" {
				continue
			}
		}
		cp = append(cp, filepath.Join(dir, filepath.FromSlash(name)))
	}

	args := []string{
		"-Xmx" + strconv.FormatUint(heapSizeLimit(info), 10),
		// ParallelGC the most adequate for high throughput and lower CPU utilization
		// It is the default GC in Java 8, but not on newer versions
		"-XX:+UseParallelGC",
		"-XX:+AlwaysActAsServerClassMachine",
		"-XX:-OmitStackTraceInFastThrow",
		"-cp", strings.Join(cp, ":"),
	}

	enableGoogleCloudProfiler := strings.Contains(options, enableGoogleCloudProfilerOption)
	enableGoogleCloudHeapSampling := strings.Contains(options, enableGoogleCloudHeapSamplingOption)
	if enableGoogleCloudProfiler {
		if metadata := info.GetMetadata(); metadata != nil {
			if jobName, nameExists := metadata["job_name"]; nameExists {
				if jobId, idExists := metadata["job_id"]; idExists {
					if enableGoogleCloudHeapSampling {
						args = append(args, fmt.Sprintf(googleCloudProfilerAgentHeapArgs, jobName, jobId))
					} else {
						args = append(args, fmt.Sprintf(googleCloudProfilerAgentBaseArgs, jobName, jobId))
					}
					log.Printf("Turning on Cloud Profiling. Profile heap: %t", enableGoogleCloudHeapSampling)
				} else {
					log.Println("Required job_id missing from metadata, profiling will not be enabled without it.")
				}
			} else {
				log.Println("Required job_name missing from metadata, profiling will not be enabled without it.")
			}
		} else {
			log.Println("enable_google_cloud_profiler is set to true, but no metadata is received from provision server, profiling will not be enabled.")
		}
	}

	disableJammAgent := strings.Contains(options, disableJammAgentOption)
	if disableJammAgent {
	  log.Printf("Disabling Jamm agent. Measuring object size will be inaccurate.")
	} else {
	  args = append(args, jammAgentArgs)
	}

	args = append(args, "org.apache.beam.fn.harness.FnHarness")
	log.Printf("Executing: java %v", strings.Join(args, " "))

	log.Fatalf("Java exited: %v", execx.Execute("java", args...))
}

// heapSizeLimit returns 80% of the runner limit, if provided. If not provided,
// it returns 70% of the physical memory on the machine. If it cannot determine
// that value, it returns 1GB. This is an imperfect heuristic. It aims to
// ensure there is memory for non-heap use and other overhead, while also not
// underutilizing the machine.
func heapSizeLimit(info *fnpb.ProvisionInfo) uint64 {
	if size, err := syscallx.PhysicalMemorySize(); err == nil {
		return (size * 70) / 100
	}
	return 1 << 30
}
