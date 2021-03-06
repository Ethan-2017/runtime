// Copyright (c) 2018 HyperHQ Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package containerdshim

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	sysexec "os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	cdruntime "github.com/containerd/containerd/runtime"
	cdshim "github.com/containerd/containerd/runtime/v2/shim"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/kata-containers/runtime/pkg/katautils"
	vc "github.com/kata-containers/runtime/virtcontainers"
	"github.com/kata-containers/runtime/virtcontainers/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/containerd/api/types/task"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	// Define the service's channel size, which is used for
	// reaping the exited processes exit state and forwarding
	// it to containerd as the containerd event format.
	bufferSize = 32

	chSize      = 128
	exitCode255 = 255
)

var (
	empty                     = &ptypes.Empty{}
	_     taskAPI.TaskService = (taskAPI.TaskService)(&service{})
)

// concrete virtcontainer implementation
var vci vc.VC = &vc.VCImpl{}

// New returns a new shim service that can be used via GRPC
func New(ctx context.Context, id string, publisher events.Publisher) (cdshim.Shim, error) {
	logger := logrus.WithField("ID", id)
	// Discard the log before shim init its log output. Otherwise
	// it will output into stdio, from which containerd would like
	// to get the shim's socket address.
	logrus.SetOutput(ioutil.Discard)
	vci.SetLogger(ctx, logger)
	katautils.SetLogger(ctx, logger, logger.Logger.Level)

	// Try to get the config file from the env KATA_CONF_FILE
	confPath := os.Getenv("KATA_CONF_FILE")
	_, runtimeConfig, err := katautils.LoadConfiguration(confPath, false, true)
	if err != nil {
		return nil, err
	}

	s := &service{
		id:         id,
		pid:        uint32(os.Getpid()),
		context:    ctx,
		config:     &runtimeConfig,
		containers: make(map[string]*container),
		events:     make(chan interface{}, chSize),
		ec:         make(chan exit, bufferSize),
	}

	go s.processExits()

	go s.forward(publisher)

	return s, nil
}

type exit struct {
	id        string
	execid    string
	pid       uint32
	status    int
	timestamp time.Time
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	sync.Mutex

	// pid Since this shimv2 cannot get the container processes pid from VM,
	// thus for the returned values needed pid, just return this shim's
	// pid directly.
	pid uint32

	context    context.Context
	sandbox    vc.VCSandbox
	containers map[string]*container
	config     *oci.RuntimeConfig
	events     chan interface{}

	ec chan exit
	id string
}

func newCommand(ctx context.Context, containerdBinary, id, containerdAddress string) (*sysexec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-namespace", ns,
		"-address", containerdAddress,
		"-publish-binary", containerdBinary,
		"-id", id,
		"-debug",
	}
	cmd := sysexec.Command(self, args...)
	cmd.Dir = cwd

	// Set the go max process to 2 in case the shim forks too much process
	cmd.Env = append(os.Environ(), "GOMAXPROCS=2")

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	return cmd, nil
}

// StartShim willl start a kata shimv2 daemon which will implemented the
// ShimV2 APIs such as create/start/update etc containers.
func (s *service) StartShim(ctx context.Context, id, containerdBinary, containerdAddress string) (string, error) {
	bundlePath, err := os.Getwd()
	if err != nil {
		return "", err
	}

	address, err := getAddress(ctx, bundlePath, id)
	if err != nil {
		return "", err
	}
	if address != "" {
		if err := cdshim.WriteAddress("address", address); err != nil {
			return "", err
		}
		return address, nil
	}

	cmd, err := newCommand(ctx, containerdBinary, id, containerdAddress)
	if err != nil {
		return "", err
	}

	address, err = cdshim.SocketAddress(ctx, id)
	if err != nil {
		return "", err
	}

	socket, err := cdshim.NewSocket(address)
	if err != nil {
		return "", err
	}
	defer socket.Close()
	f, err := socket.File()
	if err != nil {
		return "", err
	}
	defer f.Close()

	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			cmd.Process.Kill()
		}
	}()

	// make sure to wait after start
	go cmd.Wait()
	if err := cdshim.WritePidFile("shim.pid", cmd.Process.Pid); err != nil {
		return "", err
	}
	if err := cdshim.WriteAddress("address", address); err != nil {
		return "", err
	}
	return address, nil
}

func (s *service) forward(publisher events.Publisher) {
	for e := range s.events {
		if err := publisher.Publish(s.context, getTopic(s.context, e), e); err != nil {
			logrus.WithError(err).Error("post event")
		}
	}
}

func getTopic(ctx context.Context, e interface{}) string {
	switch e.(type) {
	case *eventstypes.TaskCreate:
		return cdruntime.TaskCreateEventTopic
	case *eventstypes.TaskStart:
		return cdruntime.TaskStartEventTopic
	case *eventstypes.TaskOOM:
		return cdruntime.TaskOOMEventTopic
	case *eventstypes.TaskExit:
		return cdruntime.TaskExitEventTopic
	case *eventstypes.TaskDelete:
		return cdruntime.TaskDeleteEventTopic
	case *eventstypes.TaskExecAdded:
		return cdruntime.TaskExecAddedEventTopic
	case *eventstypes.TaskExecStarted:
		return cdruntime.TaskExecStartedEventTopic
	case *eventstypes.TaskPaused:
		return cdruntime.TaskPausedEventTopic
	case *eventstypes.TaskResumed:
		return cdruntime.TaskResumedEventTopic
	case *eventstypes.TaskCheckpointed:
		return cdruntime.TaskCheckpointedEventTopic
	default:
		logrus.Warnf("no topic for type %#v", e)
	}
	return cdruntime.TaskUnknownTopic
}

func (s *service) Cleanup(ctx context.Context) (*taskAPI.DeleteResponse, error) {
	//Since the binary cleanup will return the DeleteResponse from stdout to
	//containerd, thus we must make sure there is no any outputs in stdout except
	//the returned response, thus here redirect the log to stderr in case there's
	//any log output to stdout.
	logrus.SetOutput(os.Stderr)

	if s.id == "" {
		return nil, errdefs.ToGRPCf(errdefs.ErrInvalidArgument, "the container id is empty, please specify the container id")
	}

	path, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	ociSpec, err := oci.ParseConfigJSON(path)
	if err != nil {
		return nil, err
	}

	containerType, err := ociSpec.ContainerType()
	if err != nil {
		return nil, err
	}

	switch containerType {
	case vc.PodSandbox:
		err = cleanupContainer(ctx, s.id, s.id, path)
		if err != nil {
			return nil, err
		}
	case vc.PodContainer:
		sandboxID, err := ociSpec.SandboxID()
		if err != nil {
			return nil, err
		}

		err = cleanupContainer(ctx, sandboxID, s.id, path)
		if err != nil {
			return nil, err
		}
	}

	return &taskAPI.DeleteResponse{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + uint32(unix.SIGKILL),
	}, nil
}

// Create a new sandbox or container with the underlying OCI runtime
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	s.Lock()
	defer s.Unlock()

	//the network namespace created by cni plugin
	netns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "create namespace")
	}

	rootfs := filepath.Join(r.Bundle, "rootfs")
	defer func() {
		if err != nil {
			if err2 := mount.UnmountAll(rootfs, 0); err2 != nil {
				logrus.WithError(err2).Warn("failed to cleanup rootfs mount")
			}
		}
	}()
	for _, rm := range r.Rootfs {
		m := &mount.Mount{
			Type:    rm.Type,
			Source:  rm.Source,
			Options: rm.Options,
		}
		if err := m.Mount(rootfs); err != nil {
			return nil, errors.Wrapf(err, "failed to mount rootfs component %v", m)
		}
	}

	container, err := create(ctx, s, r, netns, s.config)
	if err != nil {
		return nil, err
	}

	container.status = task.StatusCreated

	s.containers[r.ID] = container

	return &taskAPI.CreateTaskResponse{
		Pid: s.pid,
	}, nil
}

// Start a process
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	//start a container
	if r.ExecID == "" {
		err = startContainer(ctx, s, c)
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
	} else {
		//start an exec
		_, err = startExec(ctx, s, r.ID, r.ExecID)
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
	}

	return &taskAPI.StartResponse{
		Pid: s.pid,
	}, nil
}

// Delete the initial process and container
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	if r.ExecID == "" {
		err = deleteContainer(ctx, s, c)
		if err != nil {
			return nil, err
		}

		return &taskAPI.DeleteResponse{
			ExitStatus: c.exit,
			ExitedAt:   c.time,
			Pid:        s.pid,
		}, nil
	}
	//deal with the exec case
	execs, err := c.getExec(r.ExecID)
	if err != nil {
		return nil, err
	}

	delete(c.execs, r.ExecID)

	return &taskAPI.DeleteResponse{
		ExitStatus: uint32(execs.exitCode),
		ExitedAt:   execs.exitTime,
		Pid:        s.pid,
	}, nil
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	if execs := c.execs[r.ExecID]; execs != nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrAlreadyExists, "id %s", r.ExecID)
	}

	execs, err := newExec(c, r.Stdin, r.Stdout, r.Stderr, r.Terminal, r.Spec)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	c.execs[r.ExecID] = execs

	return empty, nil
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	processID := c.id
	if r.ExecID != "" {
		execs, err := c.getExec(r.ExecID)
		if err != nil {
			return nil, err
		}
		execs.tty.height = r.Height
		execs.tty.width = r.Width

		processID = execs.id

	}
	err = s.sandbox.WinsizeProcess(c.id, processID, r.Height, r.Width)
	if err != nil {
		return nil, err
	}

	return empty, err
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	if r.ExecID == "" {
		return &taskAPI.StateResponse{
			ID:         c.id,
			Bundle:     c.bundle,
			Pid:        s.pid,
			Status:     c.status,
			Stdin:      c.stdin,
			Stdout:     c.stdout,
			Stderr:     c.stderr,
			Terminal:   c.terminal,
			ExitStatus: c.exit,
		}, nil
	}

	//deal with exec case
	execs, err := c.getExec(r.ExecID)
	if err != nil {
		return nil, err
	}

	return &taskAPI.StateResponse{
		ID:         execs.id,
		Bundle:     c.bundle,
		Pid:        s.pid,
		Status:     execs.status,
		Stdin:      execs.tty.stdin,
		Stdout:     execs.tty.stdout,
		Stderr:     execs.tty.stderr,
		Terminal:   execs.tty.terminal,
		ExitStatus: uint32(execs.exitCode),
	}, nil

}

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	c.status = task.StatusPausing

	err = s.sandbox.PauseContainer(r.ID)
	if err == nil {
		c.status = task.StatusPaused
		return empty, nil
	}

	c.status, err = s.getContainerStatus(c.id)
	if err != nil {
		c.status = task.StatusUnknown
	}

	return empty, err
}

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	err = s.sandbox.ResumeContainer(c.id)
	if err == nil {
		c.status = task.StatusRunning
		return empty, nil
	}

	c.status, err = s.getContainerStatus(c.id)
	if err != nil {
		c.status = task.StatusUnknown
	}

	return empty, err
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	s.Lock()
	defer s.Unlock()

	signum := syscall.Signal(r.Signal)

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	processID := c.id
	if r.ExecID != "" {
		execs, err := c.getExec(r.ExecID)
		if err != nil {
			return nil, err
		}
		processID = execs.id
	}

	err = s.sandbox.SignalProcess(c.id, processID, signum, r.All)
	if err != nil {
		return nil, err
	}

	// Since the k8s will use the SIGTERM signal to stop a container by default, but
	// some container processes would ignore this signal such as shell, thus it's better
	// to resend another SIGKILL signal to make sure the container process terminated successfully.
	if signum == syscall.SIGTERM {
		err = s.sandbox.SignalProcess(c.id, processID, syscall.SIGKILL, r.All)
	}

	return empty, err
}

// Pids returns all pids inside the container
// Since for kata, it cannot get the process's pid from VM,
// thus only return the Shim's pid directly.
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	var processes []*task.ProcessInfo

	pInfo := task.ProcessInfo{
		Pid: s.pid,
	}
	processes = append(processes, &pInfo)

	return &taskAPI.PidsResponse{
		Processes: processes,
	}, nil
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	tty := c.ttyio
	if r.ExecID != "" {
		execs, err := c.getExec(r.ExecID)
		if err != nil {
			return nil, err
		}
		tty = execs.ttyio
	}

	if tty != nil && tty.Stdin != nil {
		if err := tty.Stdin.Close(); err != nil {
			return nil, errors.Wrap(err, "close stdin")
		}
	}

	return empty, nil
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return nil, errdefs.ToGRPCf(errdefs.ErrNotImplemented, "service Checkpoint")
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	s.Lock()
	defer s.Unlock()

	return &taskAPI.ConnectResponse{
		ShimPid: s.pid,
		//Since kata cannot get the container's pid in VM, thus only return the shim's pid.
		TaskPid: s.pid,
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	s.Lock()
	defer s.Unlock()

	if len(s.containers) != 0 {
		return empty, nil
	}

	defer os.Exit(0)

	err := s.sandbox.Stop()
	if err != nil {
		logrus.WithField("sandbox", s.sandbox.ID()).Error("failed to stop sandbox")
		return empty, err
	}

	err = s.sandbox.Delete()
	if err != nil {
		logrus.WithField("sandbox", s.sandbox.ID()).Error("failed to delete sandbox")
	}

	return empty, err
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	s.Lock()
	defer s.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	data, err := marshalMetrics(s, c.id)
	if err != nil {
		return nil, err
	}

	return &taskAPI.StatsResponse{
		Stats: data,
	}, nil
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	s.Lock()
	defer s.Unlock()

	var resources specs.LinuxResources
	if err := json.Unmarshal(r.Resources.Value, &resources); err != nil {
		return empty, err
	}

	err := s.sandbox.UpdateContainer(r.ID, resources)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return empty, nil
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	var ret uint32

	s.Lock()
	c, err := s.getContainer(r.ID)
	s.Unlock()

	if err != nil {
		return nil, err
	}

	//wait for container
	if r.ExecID == "" {
		ret = <-c.exitCh
	} else { //wait for exec
		execs, err := c.getExec(r.ExecID)
		if err != nil {
			return nil, err
		}
		ret = <-execs.exitCh
	}

	return &taskAPI.WaitResponse{
		ExitStatus: ret,
	}, nil
}

func (s *service) processExits() {
	for e := range s.ec {
		s.checkProcesses(e)
	}
}

func (s *service) checkProcesses(e exit) {
	s.Lock()
	defer s.Unlock()

	id := e.execid
	if id == "" {
		id = e.id
	}
	s.events <- &eventstypes.TaskExit{
		ContainerID: e.id,
		ID:          id,
		Pid:         e.pid,
		ExitStatus:  uint32(e.status),
		ExitedAt:    e.timestamp,
	}
	return
}

func (s *service) getContainer(id string) (*container, error) {
	c := s.containers[id]

	if c == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container does not exist %s", id)
	}

	return c, nil
}

func (s *service) getContainerStatus(containerID string) (task.Status, error) {
	cStatus, err := s.sandbox.StatusContainer(containerID)
	if err != nil {
		return task.StatusUnknown, err
	}

	var status task.Status
	switch cStatus.State.State {
	case vc.StateReady:
		status = task.StatusCreated
	case vc.StateRunning:
		status = task.StatusRunning
	case vc.StatePaused:
		status = task.StatusPaused
	case vc.StateStopped:
		status = task.StatusStopped
	}

	return status, nil
}
