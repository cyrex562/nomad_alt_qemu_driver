package alt_qemu

import (
	"context"
	"fmt"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"text/template"

	"github.com/hashicorp/consul-template/signals"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/drivers/shared/executor"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

const (
	// pluginName is the name of the plugin
	// this is used for logging and (along with the version) for uniquely
	// identifying plugin binaries fingerprinted by the client
	pluginName = "alt_qemu"

	// pluginVersion allows the client to identify and use newer versions of
	// an installed plugin
	pluginVersion = "v0.1.0"

	// fingerprintPeriod is the interval at which the plugin will send
	// fingerprint responses
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this plugin sets
	// and understands how to decode
	// this is used to allow modification and migration of the task schema
	// used by the plugin
	taskHandleVersion = 1

	qemuGracefulShutdownMsg = "system_powerdown\n"
	qemuMonitorSocketName = "qemu-monitor.sock"
	qemuLegacyMaxMonitorPathLen = 108

	// The key populated in Node Attributes to indicate presence of the Qemu driver
	driverAttr        = "driver.qemu"
	driverVersionAttr = "driver.qemu.version"
)

var (
	// pluginInfo describes the plugin
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     pluginVersion,
		Name:              pluginName,
	}

	// configSpec is the specification of the plugin's configuration
	// this is used to validate the configuration specified for the plugin
	// on the client.
	// this is not global, but can be specified on a per-client basis.
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		// TODO: define plugin's agent configuration schema.
		//
		// The schema should be defined using HCL specs and it will be used to
		// validate the agent configuration provided by the user in the
		// `plugin` stanza (https://www.nomadproject.io/docs/configuration/plugin.html).
		//
		// For example, for the schema below a valid configuration would be:
		//
		//   plugin "alt_qemu-driver-plugin" {
		//     config {
		//       shell = "fish"
		//     }
		//   }
		"image_paths": hclspec.NewAttr("image_paths", "list(string)", false),
		// TODO: what other elements are needed at the agent config level?
	})

	// taskConfigSpec is the specification of the plugin's configuration for
	// a task
	// this is used to validated the configuration specified for the plugin
	// when a job is submitted.
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		// TODO: define plugin's task configuration schema
		// TODO: identify other constants and things to add to the task config spec
		//
		// The schema should be defined using HCL specs and it will be used to
		// validate the task configuration provided by the user when they
		// submit a job.
		//
		// For example, for the schema below a valid task would be:
		//   job "example" {
		//     group "example" {
		//       task "say-hi" {
		//         driver = "alt_qemu-driver-plugin"
		//         config {
		//           greeting = "Hi"
		//         }
		//       }
		//     }
		//   }
		"image_path":        hclspec.NewAttr("image_path", "string", true),
		"accelerator":       hclspec.NewAttr("accelerator", "string", false),
		"graceful_shutdown": hclspec.NewAttr("graceful_shutdown", "bool", false),
		"args":              hclspec.NewAttr("args", "list(string)", false),
		"port_map":          hclspec.NewAttr("port_map", "list(map(number))", false),
		"qemu_system_bin":   hclspec.NewAttr("qemu_system_bin", "string", false),
		"qemu_img_bin":      hclspec.NewAttr("qemu_img_bin", "string", false),
		"vm_name": hclspec.NewAttr("vm_name", "string", false),
		"machine_type": hclspec.NewAttr("machine_type", "string", false),
	})

	// capabilities indicates what optional features this driver supports
	// this should be set according to the target run time.
	capabilities = &drivers.Capabilities{
		// TODO: set plugin's capabilities
		//
		// The plugin's capabilities signal Nomad which extra functionalities
		// are supported. For a list of available options check the docs page:
		// https://godoc.org/github.com/hashicorp/nomad/plugins/drivers#Capabilities
		SendSignals:         false,
		Exec:                false,
		FSIsolation:         drivers.FSIsolationImage,
		NetIsolationModes:   nil,
		MustInitiateNetwork: false,
	}

	versionRegex = regexp.MustCompile(`version (\d[\.\d+]+)`)
)

// Config contains configuration information for the plugin
type Config struct {
	// TODO: create decoded plugin configuration struct
	//
	// This struct is the decoded version of the schema defined in the
	// configSpec variable above. It's used to convert the HCL configuration
	// passed by the Nomad agent into Go contructs.
	ImagePaths []string `codec:"image_paths"`
}

// TaskConfig contains configuration information for a task that runs with
// this plugin
type TaskConfig struct {
	// TODO: create decoded plugin task configuration struct
	//
	// This struct is the decoded version of the schema defined in the
	// taskConfigSpec variable above. It's used to convert the string
	// configuration for the task into Go contructs.
	ImagePath        string             `codec:"image_path"`
	Accelerator      string             `codec:"accelerator"`
	Args             []string           `codec:"args"`     // extra arguments to qemu executable
	PortMap          hclutils.MapStrInt `codec:"port_map"` // A map of host port and the port name defined in the image manifest file
	GracefulShutdown bool               `codec:"graceful_shutdown"`
	QemuSystemBin    string             `codec:"qemu_system_bin"`
	QemuImgBin       string             `codec:"qemu_img_bin"`
	VmName string `codec:"vm_name"`
	MachineType string `codec:"machine_type"`
}

// TaskState is the runtime state which is encoded in the handle returned to
// Nomad client.
// This information is needed to rebuild the task state and handler during
// recovery.
type TaskState struct {
	ReattachConfig *pstructs.ReattachConfig
	TaskConfig     *drivers.TaskConfig
	StartedAt      time.Time
	Pid int

	// TODO: add any extra important values that must be persisted in order
	// to restore a task.
	//
	// The plugin keeps track of its running tasks in a in-memory data
	// structure. If the plugin crashes, this data will be lost, so Nomad
	// will respawn a new instance of the plugin and try to restore its
	// in-memory representation of the running tasks using the RecoverTask()
	// method below.
}

// AltQemuDriverPlugin is an example driver plugin. When provisioned in a job,
// the taks will output a greet specified by the user.
type AltQemuDriverPlugin struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// config is the plugin configuration set by the SetConfig RPC
	config *Config

	// nomadConfig is the client config from Nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to driver handles
	tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels
	// the ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger will log to the Nomad agent
	logger hclog.Logger
}

// NewAltQemuDriver returns a new example driver plugin
func NewAltQemuDriver(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)

	return &AltQemuDriverPlugin{
		eventer:        eventer.NewEventer(ctx, logger),
		config:         &Config{},
		tasks:          newTaskStore(),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger,
	}
}

// PluginInfo returns information describing the plugin.
func (d *AltQemuDriverPlugin) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

// ConfigSchema returns the plugin configuration schema.
func (d *AltQemuDriverPlugin) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

// SetConfig is called by the client to pass the configuration for the plugin.
func (d *AltQemuDriverPlugin) SetConfig(cfg *base.Config) error {
	var config Config
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	// Save the configuration to the plugin
	d.config = &config

	// TODO: parse and validated any configuration value if necessary.
	//
	// If your driver agent configuration requires any complex validation
	// (some dependency between attributes) or special data parsing (the
	// string "10s" into a time.Interval) you can do it here and update the
	// value in d.config.
	//

	// Save the Nomad agent configuration
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	// TODO: initialize any extra requirements if necessary.
	//
	// Here you can use the config values to initialize any resources that are
	// shared by all tasks that use this driver, such as a daemon process.

	return nil
}

// TaskConfigSchema returns the HCL schema for the configuration of a task.
func (d *AltQemuDriverPlugin) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

// Capabilities returns the features supported by the driver.
func (d *AltQemuDriverPlugin) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

// Fingerprint returns a channel that will be used to send health information
// and other driver specific node attributes.
func (d *AltQemuDriverPlugin) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

// handleFingerprint manages the channel and the flow of fingerprint data.
func (d *AltQemuDriverPlugin) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)

	// Nomad expects the initial fingerprint to be sent immediately
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			// after the initial fingerprint we can set the proper fingerprint
			// period
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

// buildFingerprint returns the driver's fingerprint data
func (d *AltQemuDriverPlugin) buildFingerprint() *drivers.Fingerprint {
	fingerprint := &drivers.Fingerprint{
		Attributes:        map[string]*pstructs.Attribute{},
		Health:            drivers.HealthStateHealthy,
		HealthDescription: drivers.DriverHealthy,
	}

	// TODO: implement fingerprinting logic to populate health and driver
	// attributes.
	//
	// Fingerprinting is used by the plugin to relay two important information
	// to Nomad: health state and node attributes.
	//
	// If the plugin reports to be unhealthy, or doesn't send any fingerprint
	// data in the expected interval of time, Nomad will restart it.
	//
	// Node attributes can be used to report any relevant information about
	// the node in which the plugin is running (specific library availability,
	// installed versions of a software etc.). These attributes can then be
	// used by an operator to set job constrains.
	//
	// In the example below we check if the shell specified by the user exists
	// in the node.
	bin := "qemu-system-x86_64"
	if runtime.GOOS == "windows" {
		// On windows, the "qemu-system-x86_64" command does not respond to the
		// version flag.
		bin = "qemu-img"
	}
	outBytes, err := exec.Command(bin, "--version").Output()
	if err != nil {
		// return no error, as it isn't an error to not find qemu, it just means we
		// can't use it.
		fingerprint.Health = drivers.HealthStateUndetected
		fingerprint.HealthDescription = ""
		return fingerprint
	}
	out := strings.TrimSpace(string(outBytes))

	matches := versionRegex.FindStringSubmatch(out)
	if len(matches) != 2 {
		fingerprint.Health = drivers.HealthStateUndetected
		fingerprint.HealthDescription = fmt.Sprintf("Failed to parse qemu version from %v", out)
		return fingerprint
	}
	currentQemuVersion := matches[1]
	fingerprint.Attributes[driverAttr] = pstructs.NewBoolAttribute(true)
	fingerprint.Attributes[driverVersionAttr] = pstructs.NewStringAttribute(currentQemuVersion)
	return fingerprint
}

// GetAbsolutePath returns the absolute path of the passed binary by resolving
// it in the path and following symlinks.
func GetAbsolutePath(bin string) (string, error) {
	lp, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path to %q executable: %v", bin, err)
	}

	return filepath.EvalSymlinks(lp)
}

// StartTask returns a task handle and a driver network if necessary.
func (d *AltQemuDriverPlugin) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var driverConfig TaskConfig

	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}

	d.logger.Info("starting task", "driver_cfg", hclog.Fmt("%+v", driverConfig))

	cfg.Env = taskenv.SetPortMapEnvs(cfg.Env, driverConfig.PortMap)

	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg

	// get the image source
	vmPath := driverConfig.ImagePath
	if vmPath == "" {
		return nil, nil, fmt.Errorf("image_path must be set")
	}

	vmID := driverConfig.VmName
	if vmID == "" {
		vmID = filepath.Base(vmPath)
	}

	if !isAllowedImagePath(d.config.ImagePaths, cfg.AllocDir, vmPath) {
		return nil, nil, fmt.Errorf("image_path is not in the allowed paths")
	}

	// parse configuration arugments
	// create the base arguments
	accelerator := "tcg"
	if driverConfig.Accelerator != "" {
		accelerator = driverConfig.Accelerator
	}

	memMb := cfg.Resources.NomadResources.Memory.MemoryMB
	if memMb < 128 || memMb > 4000000 {
		return nil, nil, fmt.Errorf("qemu memory assignment out of bounds")
	}
	mem := fmt.Sprintf("%dM", memMb)

	// TODO: this checks for a cpu share out of reasonable bounds. determine the minimum share amount and maximum
	// possible share amount. Divide the number of shares by 1000 to determine the number of vCPUs to allocate
	cpuCount := 0
	cpu := cfg.Resources.NomadResources.Cpu.CpuShares
	if cpu < 100 || cpu > 1024000 {
		return nil, nil, fmt.Errorf("cpu share assignment out of bounds")
	} else if cpu < 1000 {
		cpuCount = 1
	} else {
		cpuCount = int(cpu / 1000)
	}
	cpuCountStr := fmt.Sprintf("%d", cpuCount)

	qemuSysPath := driverConfig.QemuSystemBin
	if qemuSysPath == "" {
		qemuSysPath = "qemu-system-x86_64"
	}
	absPath, err := GetAbsolutePath(qemuSysPath)
	if err != nil {
		return nil, nil, err
	}

	machineType := driverConfig.MachineType
	if machineType == "" {
		machineType = "pc"
	}

	cpuType := driverConfig.CpuType
	if cpuType == "" {
		cpuType = "host"
	}

	// TODO: netdev type
	netdevType := "bridge"
	netdevID := "nd0"
	bootBlockDevName := "bootbd"
	bootBlockDevDriver := "qcow2"
	bootBlockDevFileDriver := "file"
	bootDeviceType := "virtio-blk"

	// TODO: options other than nographic?
	// TODO: blockdev paths, including disk format
	// TODO: vnc vs spice
	// TODO: support multiple block devs
	// TODO: support CDROM/DVD drive
	// TODO:

	args := []string {
		absPath,
		"-machine", fmt.Sprintf("type=%s,accel=%s", machineType, accelerator),
		"-name", vmID,
		"-m", mem,
		"-cpu", cpuType,
		"-smp", cpuCountStr,
		"-nographic",
		"-blockdev", fmt.Sprintf("node-name=%s,driver=#%s,file.filename=%s,file.locking=off,file.driver=%s", bootBlockDevName, bootBlockDevDriver, vmPath, bootBlockDevFileDriver),
		"-device", fmt.Sprintf("%s,drive=%s", bootDeviceType, bootBlockDevName),
		"-netdev", fmt.Sprintf("%s,id=%s", netdevType, netdevID),
		"-device", fmt.Sprintf(""),
	}

	// TODO: implement driver specific mechanism to start the task.
	//
	// Once the task is started you will need to store any relevant runtime
	// information in a taskHandle and TaskState. The taskHandle will be
	// stored in-memory in the plugin and will be used to interact with the
	// task.
	//
	// The TaskState will be returned to the Nomad client inside a
	// drivers.TaskHandle instance. This TaskHandle will be sent back to plugin
	// if the task ever needs to be recovered, so the TaskState should contain
	// enough information to handle that.
	//
	// In the example below we use an executor to fork a process to run our
	// greeter. The executor is then stored in the handle so we can access it
	// later and the the plugin.Client is used to generate a reattach
	// configuration that can be used to recover communication with the task.
	executorConfig := &executor.ExecutorConfig{
		LogFile:  filepath.Join(cfg.TaskDir().Dir, "executor.out"),
		LogLevel: "debug",
	}

	exec, pluginClient, err := executor.CreateExecutor(d.logger, d.nomadConfig, executorConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create executor: %v", err)
	}

	echoCmd := fmt.Sprintf(`echo "%s"`, driverConfig.Greeting)
	execCmd := &executor.ExecCommand{
		Cmd:        d.config.Shell,
		Args:       []string{"-c", echoCmd},
		StdoutPath: cfg.StdoutPath,
		StderrPath: cfg.StderrPath,
	}

	ps, err := exec.Launch(execCmd)
	if err != nil {
		pluginClient.Kill()
		return nil, nil, fmt.Errorf("failed to launch command with executor: %v", err)
	}

	h := &taskHandle{
		exec:         exec,
		pid:          ps.Pid,
		pluginClient: pluginClient,
		taskConfig:   cfg,
		procState:    drivers.TaskStateRunning,
		startedAt:    time.Now().Round(time.Millisecond),
		logger:       d.logger,
	}

	driverState := TaskState{
		ReattachConfig: structs.ReattachConfigFromGoPlugin(pluginClient.ReattachConfig()),
		Pid:            ps.Pid,
		TaskConfig:     cfg,
		StartedAt:      h.startedAt,
	}

	if err := handle.SetDriverState(&driverState); err != nil {
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.tasks.Set(cfg.ID, h)
	go h.run()
	return handle, nil, nil
}

// RecoverTask recreates the in-memory state of a task from a TaskHandle.
func (d *AltQemuDriverPlugin) RecoverTask(handle *drivers.TaskHandle) error {
	if handle == nil {
		return fmt.Errorf("error: handle cannot be nil")
	}

	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		d.logger.Trace("nothing to recover; task already exists",
			"task_id", handle.Config.ID,
			"task_name", handle.Config.Name,
			)
		return nil
	}

	var taskState TaskState
	if err := handle.GetDriverState(&taskState); err != nil {
		msg := fmt.Sprintf("failed to decode taskConfig state from handle: %v", err)
		d.logger.Error(msg, "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf(msg)
	}

	var driverConfig TaskConfig
	if err := taskState.TaskConfig.DecodeDriverConfig(&driverConfig); err != nil {
		msg := fmt.Sprintf("faield to decode driver config: #{err}")
		d.logger.Error(msg, "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf("failed to decode driver config: %v", err)
	}

	// TODO: implement driver specific logic to recover a task.
	//
	// Recovering a task involves recreating and storing a taskHandle as if the
	// task was just started.
	//
	// In the example below we use the executor to re-attach to the process
	// that was created when the task first started.
	plugRC, err := pstructs.ReattachConfigToGoPlugin(taskState.ReattachConfig)
	if err != nil {
		msg := fmt.Sprintf("failed to build ReattachConfig from taskConfig state: #{err}")
		d.logger.Error(msg, "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf(msg)
	}

	execImpl, pluginClient, err := executor.ReattachToExecutor(plugRC, d.logger.With("task_name", handle.Config.Name, "alloc_id", handle.Config.AllocID))
	if err != nil {
		d.logger.Error("failed to reattach to executor", "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf("failed to reattach to executor: #{err}")
	}

	h := &taskHandle{
		exec:         execImpl,
		pid:          taskState.Pid,
		pluginClient: pluginClient,
		taskConfig:   taskState.TaskConfig,
		procState:    drivers.TaskStateRunning,
		startedAt:    taskState.StartedAt,
		exitResult:   &drivers.ExitResult{},
		logger: d.logger,
	}

	d.tasks.Set(taskState.TaskConfig.ID, h)

	go h.run()
	return nil
}

func isAllowedImagePath(allowedPaths []string, allocDir, imagePath string) bool {
	if !filepath.IsAbs(imagePath) {
		imagePath = filepath.Join(allocDir, imagePath)
	}

	isParent := func(parent, path string) bool {
		rel, err := filepath.Rel(parent, path)
		return err == nill && !strings.HasPrefix(rel, "..")
	}

	if isParent(allocDir, imagePath) {
		return true
	}

	for _, ap := range allowedPaths {
		if isParent(ap, imagePath) {
			return true
		}
	}

	return false
}

// WaitTask returns a channel used to notify Nomad when a task exits.
func (d *AltQemuDriverPlugin) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)
	return ch, nil
}

func (d *AltQemuDriverPlugin) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
	defer close(ch)
	var result *drivers.ExitResult

	// TODO: implement driver specific logic to notify Nomad the task has been
	// completed and what was the exit result.
	//
	// When a result is sent in the result channel Nomad will stop the task and
	// emit an event that an operator can use to get an insight on why the task
	// stopped.
	//
	// In the example below we block and wait until the executor finishes
	// running, at which point we send the exit code and signal in the result
	// channel.
	ps, err := handle.exec.Wait(ctx)
	if err != nil {
		result = &drivers.ExitResult{
			Err: fmt.Errorf("executor: error waiting on process: %v", err),
		}
	} else {
		result = &drivers.ExitResult{
			ExitCode: ps.ExitCode,
			Signal:   ps.Signal,
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case ch <- result:
		}
	}
}

// StopTask stops a running task with the given signal and within the timeout window.
func (d *AltQemuDriverPlugin) StopTask(taskID string, timeout time.Duration, signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	// TODO: implement driver specific logic to stop a task.
	//
	// The StopTask function is expected to stop a running task by sending the
	// given signal to it. If the task does not stop during the given timeout,
	// the driver must forcefully kill the task.
	//
	// In the example below we let the executor handle the task shutdown
	// process for us, but you might need to customize this for your own
	// implementation.
	if err := handle.exec.Shutdown(signal, timeout); err != nil {
		if handle.pluginClient.Exited() {
			return nil
		}
		return fmt.Errorf("executor Shutdown failed: %v", err)
	}

	return nil
}

// DestroyTask cleans up and removes a task that has terminated.
func (d *AltQemuDriverPlugin) DestroyTask(taskID string, force bool) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.IsRunning() && !force {
		return fmt.Errorf("cannot destroy running task")
	}

	// TODO: implement driver specific logic to destroy a complete task.
	//
	// Destroying a task includes removing any resources used by task and any
	// local references in the plugin. If force is set to true the task should
	// be destroyed even if it's currently running.
	//
	// In the example below we use the executor to force shutdown the task
	// (timeout equals 0).
	if !handle.pluginClient.Exited() {
		if err := handle.exec.Shutdown("", 0); err != nil {
			handle.logger.Error("destroying executor failed", "err", err)
		}

		handle.pluginClient.Kill()
	}

	d.tasks.Delete(taskID)
	return nil
}

// InspectTask returns detailed status information for the referenced taskID.
func (d *AltQemuDriverPlugin) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

// TaskStats returns a channel which the driver should send stats to at the given interval.
func (d *AltQemuDriverPlugin) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	// TODO: implement driver specific logic to send task stats.
	//
	// This function returns a channel that Nomad will use to listen for task
	// stats (e.g., CPU and memory usage) in a given interval. It should send
	// stats until the context is canceled or the task stops running.
	//
	// In the example below we use the Stats function provided by the executor,
	// but you can build a set of functions similar to the fingerprint process.
	return handle.exec.Stats(ctx, interval)
}

// TaskEvents returns a channel that the plugin can use to emit task related events.
func (d *AltQemuDriverPlugin) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

// SignalTask forwards a signal to a task.
// This is an optional capability.
func (d *AltQemuDriverPlugin) SignalTask(taskID string, signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	// TODO: implement driver specific signal handling logic.
	//
	// The given signal must be forwarded to the target taskID. If this plugin
	// doesn't support receiving signals (capability SendSignals is set to
	// false) you can just return nil.
	sig := os.Interrupt
	if s, ok := signals.SignalLookup[signal]; ok {
		sig = s
	} else {
		d.logger.Warn("unknown signal to send to task, using SIGINT instead", "signal", signal, "task_id", handle.taskConfig.ID)

	}
	return handle.exec.Signal(sig)
}

// ExecTask returns the result of executing the given command inside a task.
// This is an optional capability.
func (d *AltQemuDriverPlugin) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	// TODO: implement driver specific logic to execute commands in a task.
	return nil, fmt.Errorf("This driver does not support exec")
}
