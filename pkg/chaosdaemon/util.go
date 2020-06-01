// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package chaosdaemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/containerd/containerd/runtime/v2/task"
	"github.com/docker/docker/daemon/cluster/executor/container"
	"google.golang.org/grpc"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"

	"github.com/containerd/containerd"
	"github.com/docker/docker/api/types"
	dockerclient "github.com/docker/docker/client"
	cri "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"

	"github.com/pingcap/chaos-mesh/pkg/mock"
	"github.com/pingcap/chaos-mesh/pkg/utils"
)

const (
	containerRuntimeDocker     = "docker"
	containerRuntimeContainerd = "containerd"
	containerRuntimeCRIO = "crio"

	defaultDockerSocket  = "unix:///var/run/docker.sock"
	dockerProtocolPrefix = "docker://"

	// TODO(yeya24): make socket and ns configurable
	defaultContainerdSocket  = "/run/containerd/containerd.sock"
	containerdProtocolPrefix = "containerd://"
	containerdDefaultNS      = "k8s.io"

	crioProtocolPrefix = "crio://"
	defaultCRIOSocket = ""

	defaultProcPrefix = "/proc"
)

// ContainerRuntimeInfoClient represents a struct which can give you information about container runtime
type ContainerRuntimeInfoClient interface {
	GetPidFromContainerID(ctx context.Context, containerID string) (uint32, error)
	ContainerKillByContainerID(ctx context.Context, containerID string) error
	FormatContainerID(ctx context.Context, containerID string) (string, error)
}

// DockerClientInterface represents the DockerClient, it's used to simplify unit test
type DockerClientInterface interface {
	ContainerInspect(ctx context.Context, containerID string) (types.ContainerJSON, error)
	ContainerKill(ctx context.Context, containerID, signal string) error
}

// DockerClient can get information from docker
type DockerClient struct {
	client DockerClientInterface
}

// FormatContainerID strips protocol prefix from the container ID
func (c DockerClient) FormatContainerID(ctx context.Context, containerID string) (string, error) {
	if len(containerID) < len(dockerProtocolPrefix) {
		return "", fmt.Errorf("container id %s is not a docker container id", containerID)
	}
	if containerID[0:len(dockerProtocolPrefix)] != dockerProtocolPrefix {
		return "", fmt.Errorf("expected %s but got %s", dockerProtocolPrefix, containerID[0:len(dockerProtocolPrefix)])
	}
	return containerID[len(dockerProtocolPrefix):], nil
}

// GetPidFromContainerID fetches PID according to container id
func (c DockerClient) GetPidFromContainerID(ctx context.Context, containerID string) (uint32, error) {
	id, err := c.FormatContainerID(ctx, containerID)
	if err != nil {
		return 0, err
	}
	container, err := c.client.ContainerInspect(ctx, id)
	if err != nil {
		return 0, err
	}

	return uint32(container.State.Pid), nil
}

// ContainerdClientInterface represents the ContainerClient, it's used to simply unit test
type ContainerdClientInterface interface {
	LoadContainer(ctx context.Context, id string) (containerd.Container, error)
}

// ContainerdClient can get information from containerd
type ContainerdClient struct {
	client ContainerdClientInterface
}

// FormatContainerID strips protocol prefix from the container ID
func (c ContainerdClient) FormatContainerID(ctx context.Context, containerID string) (string, error) {
	if len(containerID) < len(containerdProtocolPrefix) {
		return "", fmt.Errorf("container id %s is not a containerd container id", containerID)
	}
	if containerID[0:len(containerdProtocolPrefix)] != containerdProtocolPrefix {
		return "", fmt.Errorf("expected %s but got %s", containerdProtocolPrefix, containerID[0:len(containerdProtocolPrefix)])
	}
	return containerID[len(containerdProtocolPrefix):], nil
}

// GetPidFromContainerID fetches PID according to container id
func (c ContainerdClient) GetPidFromContainerID(ctx context.Context, containerID string) (uint32, error) {
	id, err := c.FormatContainerID(ctx, containerID)
	if err != nil {
		return 0, err
	}
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return 0, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return 0, err
	}
	return task.Pid(), nil
}

// newDockerclient returns a dockerclient.NewClient with mock points
func newDockerClient(host string, version string, client *http.Client, httpHeaders map[string]string) (DockerClientInterface, error) {
	// Mock point to return error or mock client in unit test
	if err := mock.On("NewDockerClientError"); err != nil {
		return nil, err.(error)
	}
	if client := mock.On("MockDockerClient"); client != nil {
		return client.(DockerClientInterface), nil
	}

	// The real logic
	return dockerclient.NewClient(host, version, client, httpHeaders)
}

// newContainerdClient returns a containerd.New with mock points
func newContainerdClient(address string, opts ...containerd.ClientOpt) (ContainerdClientInterface, error) {
	// Mock point to return error in unit test
	if err := mock.On("NewContainerdClientError"); err != nil {
		return nil, err.(error)
	}
	if client := mock.On("MockContainerdClient"); client != nil {
		return client.(ContainerdClientInterface), nil
	}

	// The real logic
	return containerd.New(address, opts...)
}

type CRIClient struct {
	client cri.RuntimeServiceClient
}

// newCRIClient returns a CRIO client
func newCRIClient(address string) (*CRIClient, error) {
	conn, err :=
	if err != nil {
		return nil, err
	}

	return &CRIClient{
		cri.NewRuntimeServiceClient(conn),
	}, nil
}

// FormatContainerID strips protocol prefix from the container ID
func (c CRIClient) FormatContainerID(ctx context.Context, containerID string) (string, error) {
	if len(containerID) < len(crioProtocolPrefix) {
		return "", fmt.Errorf("container id %s is not a crio container id", containerID)
	}
	if containerID[0:len(crioProtocolPrefix)] != crioProtocolPrefix {
		return "", fmt.Errorf("expected %s but got %s", crioProtocolPrefix, containerID[0:len(crioProtocolPrefix)])
	}
	return containerID[len(crioProtocolPrefix):], nil
}

// GetPidFromContainerID fetches PID according to container id
func (c CRIClient) GetPidFromContainerID(ctx context.Context, containerID string) (uint32, error) {
	id, err := c.FormatContainerID(ctx, containerID)
	if err != nil {
		return 0, err
	}
	res, err := c.client.ContainerStatus(ctx, cri.ContainerStatusRequest{ContainerId: containerID})
	if err != nil {
		return 0, err
	}

	info := res.Info
	pidString, ok := info["pid"]
	if !ok {
		// this should not happen
		return 0, errors.New("no pid from CRI response")
	}

	pid, err := strconv.Atoi(pidString)
	if err != nil {
		return 0, err
	}

	return pid, nil
}

// CreateContainerRuntimeInfoClient creates a container runtime information client.
func CreateContainerRuntimeInfoClient(containerRuntime string) (ContainerRuntimeInfoClient, error) {
	// TODO: support more container runtime

	var cli ContainerRuntimeInfoClient
	switch containerRuntime {
	case containerRuntimeDocker:
		client, err := newDockerClient(defaultDockerSocket, "", nil, nil)
		if err != nil {
			return nil, err
		}
		cli = DockerClient{client}

	case containerRuntimeContainerd:
		// TODO(yeya24): add more options?
		client, err := newContainerdClient(defaultContainerdSocket, containerd.WithDefaultNamespace(containerdDefaultNS))
		if err != nil {
			return nil, err
		}
		cli = ContainerdClient{client}

	case containerRuntimeCRIO:
		conn, err := grpc.Dial(defaultCRIOSocket, grpc.WithInsecure(), grpc.WithBlock(),
			grpc.WithUnaryInterceptor(utils.TimeoutClientInterceptor))
		if err != nil {
			return nil, err
		cli = CRIClient{client: cri.NewRuntimeServiceClient(conn)}

	default:
		return nil, fmt.Errorf("only docker and containerd is supported, but got %s", containerRuntime)
	}

	return cli, nil
}

// GetNetnsPath returns network namespace path
func GenNetnsPath(pid uint32) string {
	return fmt.Sprintf("%s/%d/ns/net", defaultProcPrefix, pid)
}

func withNetNS(ctx context.Context, nsPath string, cmd string, args ...string) *exec.Cmd {
	// Mock point to return mock Cmd in unit test
	if c := mock.On("MockWithNetNs"); c != nil {
		f := c.(func(context.Context, string, string, ...string) *exec.Cmd)
		return f(ctx, nsPath, cmd, args...)
	}

	// BusyBox's nsenter is very confusing. This usage is found by several attempts
	args = append([]string{"-n" + nsPath, "--", cmd}, args...)

	return exec.CommandContext(ctx, "nsenter", args...)
}

// ContainerKillByContainerID kills container according to container id
func (c DockerClient) ContainerKillByContainerID(ctx context.Context, containerID string) error {
	id, err := c.FormatContainerID(ctx, containerID)
	if err != nil {
		return err
	}

	return c.client.ContainerKill(ctx, id, "SIGKILL")
}

// ContainerKillByContainerID kills container according to container id
func (c ContainerdClient) ContainerKillByContainerID(ctx context.Context, containerID string) error {
	id, err := c.FormatContainerID(ctx, containerID)
	if err != nil {
		return err
	}
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return err
	}

	return task.Kill(ctx, syscall.SIGKILL)
}

// ContainerKillByContainerID kills container according to container id
func (c CRIClient) ContainerKillByContainerID(ctx context.Context, containerID string) error {
	id, err := c.FormatContainerID(ctx, containerID)
	if err != nil {
		return err
	}
	_, err = c.client.RemoveContainer(ctx, &cri.RemoveContainerRequest{ContainerId: id})
	return err
}

// GetChildProcesses will return all child processes's pid. Include all generations.
func GetChildProcesses(ppid uint32) ([]uint32, error) {
	procs, err := ioutil.ReadDir(defaultProcPrefix)
	if err != nil {
		return nil, err
	}

	type processPair struct {
		Pid  uint32
		Ppid uint32
	}

	pairs := make(chan processPair)
	done := make(chan bool)

	go func() {
		var wg sync.WaitGroup

		for _, proc := range procs {
			_, err := strconv.ParseUint(proc.Name(), 10, 32)
			if err != nil {
				continue
			}

			statusPath := defaultProcPrefix + "/" + proc.Name() + "/stat"

			wg.Add(1)
			go func() {
				defer wg.Done()

				reader, err := os.Open(statusPath)
				if err != nil {
					log.Error(err, "read status file error", "path", statusPath)
					return
				}

				var (
					pid    uint32
					comm   string
					state  string
					parent uint32
				)
				// according to procfs's man page
				fmt.Fscanf(reader, "%d %s %s %d", &pid, &comm, &state, &parent)

				pairs <- processPair{
					Pid:  pid,
					Ppid: parent,
				}
			}()
		}

		wg.Wait()
		done <- true
	}()

	processGraph := utils.NewGraph()
	for {
		select {
		case pair := <-pairs:
			processGraph.Insert(pair.Ppid, pair.Pid)
		case <-done:
			return processGraph.Flatten(ppid), nil
		}
	}
}
