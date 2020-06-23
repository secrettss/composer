/*
   Copyright 2020 Docker, Inc.

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

package containers

import (
	"context"
	"io"
)

// Container represents a created container
type Container struct {
	ID          string
	Status      string
	Image       string
	Command     string
	CPUTime     uint64
	MemoryUsage uint64
	MemoryLimit uint64
	PidsCurrent uint64
	PidsLimit   uint64
	Labels      []string
	Ports       []Port
}

// Port represents a published port of a container
type Port struct {
	// HostPort is the port number on the host
	HostPort uint32
	// ContainerPort is the port number inside the container
	ContainerPort uint32
	// Protocol is the protocol of the port mapping
	Protocol string
	// HostIP is the host ip to use
	HostIP string
}

// ContainerConfig contains the configuration data about a container
type ContainerConfig struct {
	// ID uniquely identifies the container
	ID string
	// Image specifies the iamge reference used for a container
	Image string
	// Ports provide a list of published ports
	Ports []Port
	// Labels set labels to the container
	Labels map[string]string
	// Volumes to be mounted
	Volumes []string
}

// LogsRequest contains configuration about a log request
type LogsRequest struct {
	Follow bool
	Tail   string
	Writer io.Writer
}

// Service interacts with the underlying container backend
type Service interface {
	// List returns all the containers
	List(ctx context.Context, all bool) ([]Container, error)
	// Stop stops the running container
	Stop(ctx context.Context, containerID string, timeout *uint32) error
	// Run creates and starts a container
	Run(ctx context.Context, config ContainerConfig) error
	// Exec executes a command inside a running container
	Exec(ctx context.Context, containerName string, command string, reader io.Reader, writer io.Writer) error
	// Logs returns all the logs of a container
	Logs(ctx context.Context, containerName string, request LogsRequest) error
	// Delete removes containers
	Delete(ctx context.Context, id string, force bool) error
	// Inspect get a specific container
	Inspect(ctx context.Context, id string) (Container, error)
}