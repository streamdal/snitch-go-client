// Package snitch is a library that allows running of Client data pipelines against data
// This package is designed to be included in golang message bus libraries. The only public
// method is Process() which is used to run pipelines against data.
//
// Use of this package requires a running instance of a snitch server.
// The server can be downloaded at https://github.com/streamdal/snitch
//
// The following environment variables must be set:
// - SNITCH_URL: The address of the Client server
// - SNITCH_TOKEN: The token to use when connecting to the Client server
//
// Optional parameters:
// - SNITCH_DRY_RUN: If true, rule hits will only be logged, no failure modes will be ran
package snitch

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/relistan/go-director"

	"github.com/streamdal/snitch-protos/build/go/protos"

	"github.com/streamdal/snitch-go-client/logger"
	"github.com/streamdal/snitch-go-client/metrics"
	"github.com/streamdal/snitch-go-client/server"
	"github.com/streamdal/snitch-go-client/types"
)

// OperationType is a constant that represents whether we are publishing or consuming,
// it must match the protobuf enum of the rule
type OperationType int

type ClientType int

const (
	// Produce tells Process to run the pipelines against the produce ruleset
	Produce OperationType = 1

	// Consume tells Process to run the pipelines against the consume ruleset
	Consume OperationType = 2

	// RuleUpdateInterval is how often to check for rule updates
	RuleUpdateInterval = time.Second * 30

	// ReconnectSleep determines the length of time to wait between reconnect attempts to snitch server
	ReconnectSleep = time.Second * 5

	// MaxPayloadSize is the maximum size of data that can be sent to the WASM module
	MaxPayloadSize = 1024 * 1024 // 1Mi

	ClientTypeSDK  ClientType = 1
	ClientTypeShim ClientType = 2
)

var (
	ErrEmptyConfig        = errors.New("config cannot be empty")
	ErrEmptyServiceName   = errors.New("data source cannot be empty")
	ErrMissingShutdownCtx = errors.New("shutdown context cannot be nil")

	// ErrMessageDropped is returned when a message is dropped by the plumber data pipelines
	// An end user may check for this error and handle it accordingly in their code
	//ErrMessageDropped = errors.New("message dropped by plumber data pipelines")

	ErrEmptyCommand = errors.New("command cannot be empty")
)

type ISnitch interface {
	ApplyRules(ctx context.Context, mode OperationType, key string, data []byte) ([]byte, error)
}

type Snitch struct {
	config             *Config
	functions          map[string]*function
	pipelines          map[string]map[string]*protos.Command
	pipelinesPaused    map[string]map[string]*protos.Command
	functionsMtx       *sync.RWMutex
	pipelinesMtx       *sync.RWMutex
	pipelinesPausedMtx *sync.RWMutex
	serverClient       server.IServerClient
	metrics            metrics.IMetrics
	audiences          map[string]struct{}
	audiencesMtx       *sync.RWMutex
	sessionID          string
}

type Config struct {
	SnitchURL       string
	SnitchToken     string
	ServiceName     string
	PipelineTimeout time.Duration
	StepTimeout     time.Duration
	DryRun          bool
	ShutdownCtx     context.Context
	Logger          logger.Logger
	ClientType      ClientType
	Audiences       []*Audience
}

type Audience struct {
	ServiceName   string
	ComponentName string
	OperationType OperationType
	OperationName string
}

type ProcessRequest struct {
	ComponentName string
	OperationType OperationType
	OperationName string
	Data          []byte
}

type ProcessResponse struct {
	Data    []byte
	Error   bool
	Message string
}

func New(cfg *Config) (*Snitch, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, errors.Wrap(err, "unable to validate config")
	}

	// We instantiate this library based on whether or not we have a Client URL+token
	// If these are not provided, the wrapper library will not perform rule checks and
	// will act as normal
	if cfg.SnitchURL == "" || cfg.SnitchToken == "" {
		return nil, nil
	}

	serverClient, err := server.New(cfg.SnitchURL, cfg.SnitchToken)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to connect to snitch server '%s'", cfg.SnitchURL)
	}

	m, err := metrics.New(&metrics.Config{
		ServerClient: serverClient,
		ShutdownCtx:  cfg.ShutdownCtx,
		Log:          cfg.Logger,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to start metrics service")
	}

	s := &Snitch{
		functions:          make(map[string]*function),
		functionsMtx:       &sync.RWMutex{},
		serverClient:       serverClient,
		pipelines:          make(map[string]map[string]*protos.Command),
		pipelinesMtx:       &sync.RWMutex{},
		pipelinesPaused:    make(map[string]map[string]*protos.Command),
		pipelinesPausedMtx: &sync.RWMutex{},
		audiences:          map[string]struct{}{},
		audiencesMtx:       &sync.RWMutex{},
		config:             cfg,
		metrics:            m,
		sessionID:          uuid.New().String(),
	}

	if cfg.DryRun {
		cfg.Logger.Warn("data pipelines running in dry run mode")
	}

	cmds, err := s.serverClient.GetAttachCommandsByService(context.Background(), cfg.ServiceName)
	for _, cmd := range cmds {
		cfg.Logger.Debugf("Attaching pipeline '%s'", cmd.GetAttachPipeline().Pipeline.Name)
		if err := s.attachPipeline(context.Background(), cmd); err != nil {
			cfg.Logger.Errorf("failed to attach pipeline: %s", err)
		}
	}

	// Start register
	go s.register(director.NewFreeLooper(director.FOREVER, make(chan error, 1)))

	// Start heartbeat
	go s.heartbeat(director.NewTimedLooper(director.FOREVER, time.Second, make(chan error, 1)))

	return s, nil
}

func validateConfig(cfg *Config) error {
	if cfg == nil {
		return ErrEmptyConfig
	}

	if cfg.ShutdownCtx == nil {
		return ErrMissingShutdownCtx
	}

	if cfg.ServiceName == "" {
		cfg.ServiceName = os.Getenv("SNITCH_SERVICE_NAME")
		if cfg.ServiceName == "" {
			return ErrEmptyServiceName
		}
	}

	// Can be specified in config for lib use, or via envar for shim use
	if cfg.SnitchURL == "" {
		cfg.SnitchURL = os.Getenv("SNITCH_URL")
	}

	// Can be specified in config for lib use, or via envar for shim use
	if cfg.SnitchToken == "" {
		cfg.SnitchToken = os.Getenv("SNITCH_TOKEN")
	}

	// Can be specified in config for lib use, or via envar for shim use
	if os.Getenv("SNITCH_DRY_RUN") == "true" {
		cfg.DryRun = true
	}

	// Can be specified in config for lib use, or via envar for shim use
	if cfg.StepTimeout == 0 {
		to := os.Getenv("SNITCH_STEP_TIMEOUT")
		if to == "" {
			to = "1s"
		}

		timeout, err := time.ParseDuration(to)
		if err != nil {
			return errors.Wrap(err, "unable to parse SNITCH_STEP_TIMEOUT")
		}
		cfg.StepTimeout = timeout
	}

	// Can be specified in config for lib use, or via envar for shim use
	if cfg.PipelineTimeout == 0 {
		to := os.Getenv("SNITCH_PIPELINE_TIMEOUT")
		if to == "" {
			to = "1s"
		}

		timeout, err := time.ParseDuration(to)
		if err != nil {
			return errors.Wrap(err, "unable to parse SNITCH_PIPELINE_TIMEOUT")
		}
		cfg.PipelineTimeout = timeout
	}

	// Default to NOOP logger if none is provided
	if cfg.Logger == nil {
		cfg.Logger = &logger.NoOpLogger{}
	}

	return nil
}

func (s *Snitch) heartbeat(loop *director.TimedLooper) {
	var quit bool
	loop.Loop(func() error {
		if quit {
			time.Sleep(time.Millisecond * 50)
			return nil
		}

		select {
		case <-s.config.ShutdownCtx.Done():
			quit = true
			loop.Quit()
			return nil
		default:
			// NOOP
		}

		if err := s.serverClient.HeartBeat(s.config.ShutdownCtx, s.sessionID); err != nil {
			s.config.Logger.Errorf("failed to send heartbeat: %s", err)
		}

		return nil
	})
}

func (s *Snitch) runStep(ctx context.Context, step *protos.PipelineStep, data []byte) (*protos.WASMResponse, error) {
	s.config.Logger.Debugf("Running step '%s'", step.Name)
	// Get WASM module
	f, err := s.getFunction(ctx, step)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get wasm data")
	}

	// Don't need this anymore, and don't want to send it to the wasm function
	step.XWasmBytes = nil

	req := &protos.WASMRequest{
		Input: data,
		Step:  step,
	}

	reqBytes, err := proto.Marshal(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal WASM request")
	}

	// Run WASM module
	respBytes, err := f.Exec(ctx, reqBytes)
	if err != nil {
		return nil, errors.Wrap(err, "failed to execute wasm module")
	}

	resp := &protos.WASMResponse{}
	if err := proto.Unmarshal(respBytes, resp); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal WASM response")
	}

	return resp, nil
}

func (s *Snitch) getPipelines(ctx context.Context, aud *protos.Audience) map[string]*protos.Command {
	s.pipelinesMtx.RLock()
	defer s.pipelinesMtx.RUnlock()

	pipelines, ok := s.pipelines[audToStr(aud)]
	if !ok {
		// No pipelines for this audience key
		// But we should notify snitch server we've seen it
		s.addAudience(ctx, aud)

		return make(map[string]*protos.Command)
	}

	return pipelines
}

func (s *Snitch) Process(ctx context.Context, req *ProcessRequest) (*ProcessResponse, error) {
	if req == nil {
		return nil, errors.New("request cannot be nil")
	}

	data := req.Data
	payloadSize := int64(len(data))

	aud := &protos.Audience{
		ServiceName:   s.config.ServiceName,
		ComponentName: req.ComponentName,
		OperationType: protos.OperationType(req.OperationType),
		OperationName: req.OperationName,
	}

	labels := map[string]string{
		"service":        s.config.ServiceName,
		"component_name": req.ComponentName,
		"operation_name": req.OperationName,
		"pipeline_name":  "",
		"pipeline_id":    "",
	}

	counterError := types.ConsumeErrorCount
	counterProcessed := types.ConsumeProcessedCount
	counterBytes := types.ConsumeBytes
	if req.OperationType == Produce {
		counterError = types.ProduceErrorCount
		counterProcessed = types.ProduceProcessedCount
		counterBytes = types.ProduceBytes
	}

	pipelines := s.getPipelines(ctx, aud)
	if len(pipelines) == 0 {
		// No pipelines for this mode, nothing to do
		return &ProcessResponse{Data: data, Message: "No pipelines, message ignored"}, nil
	}

	if payloadSize > MaxPayloadSize {

		_ = s.metrics.Incr(ctx, &types.CounterEntry{Name: counterError, Labels: labels, Value: 1})

		msg := fmt.Sprintf("data size exceeds maximum, skipping pipelines on audience %s", audToStr(aud))
		s.config.Logger.Warn(msg)
		return &ProcessResponse{Data: data, Error: true, Message: msg}, nil
	}

	for _, p := range pipelines {
		pipeline := p.GetAttachPipeline().GetPipeline()
		labels["pipeline_name"] = pipeline.Name
		labels["pipeline_id"] = pipeline.Id

		_ = s.metrics.Incr(ctx, &types.CounterEntry{Name: counterProcessed, Labels: labels, Value: 1})
		_ = s.metrics.Incr(ctx, &types.CounterEntry{Name: counterBytes, Labels: labels, Value: payloadSize})

		for _, step := range pipeline.Steps {
			wasmResp, err := s.runStep(ctx, step, data)
			if err != nil {
				s.config.Logger.Errorf("failed to run step '%s': %s", step.Name, err)
				shouldContinue := s.handleConditions(ctx, step.OnFailure, pipeline, step, aud, req)
				if !shouldContinue {
					return &ProcessResponse{
						Data:    req.Data,
						Error:   true,
						Message: err.Error(),
					}, nil
				}

				// wasmResp will be nil, so don't allow code below to execute
				continue
			}

			// Check on success and on-failures
			switch wasmResp.ExitCode {
			case protos.WASMExitCode_WASM_EXIT_CODE_SUCCESS:
				s.config.Logger.Debugf("Step '%s' returned exit code success", step.Name)
				shouldContinue := s.handleConditions(ctx, step.OnSuccess, pipeline, step, aud, req)
				if !shouldContinue {
					return &ProcessResponse{
						Data:    wasmResp.Output,
						Error:   false,
						Message: "",
					}, nil
				}
			case protos.WASMExitCode_WASM_EXIT_CODE_FAILURE:
				s.config.Logger.Errorf("Step '%s' returned exit code failure", step.Name)
				shouldContinue := s.handleConditions(ctx, step.OnFailure, pipeline, step, aud, req)
				if !shouldContinue {
					return &ProcessResponse{
						Data:    wasmResp.Output,
						Error:   true,
						Message: "detective step failed", // TODO: WASM module should return the error message, not just "detective run completed"
					}, nil
				}
			case protos.WASMExitCode_WASM_EXIT_CODE_INTERNAL_ERROR:
				s.config.Logger.Errorf("Step '%s' returned exit code internal error", step.Name)
				shouldContinue := s.handleConditions(ctx, step.OnFailure, pipeline, step, aud, req)
				if !shouldContinue {
					return &ProcessResponse{
						Data:    wasmResp.Output,
						Error:   true,
						Message: "detective step failed:" + wasmResp.ExitMsg,
					}, nil
				}
			default:
				s.config.Logger.Debugf("Step '%s' returned unknown exit code %d", step.Name, wasmResp.ExitCode)
			}

			data = wasmResp.Output
		}
	}

	// Dry run should not modify anything, but we must allow pipeline to
	// mutate internal state in order to function properly
	if s.config.DryRun {
		data = req.Data
	}

	return &ProcessResponse{
		Data:    data,
		Error:   false,
		Message: "",
	}, nil
}

func (s *Snitch) handleConditions(
	ctx context.Context,
	conditions []protos.PipelineStepCondition,
	pipeline *protos.Pipeline,
	step *protos.PipelineStep,
	aud *protos.Audience,
	req *ProcessRequest,
) bool {
	shouldContinue := true
	for _, condition := range conditions {
		switch condition {
		case protos.PipelineStepCondition_PIPELINE_STEP_CONDITION_NOTIFY:
			s.config.Logger.Debugf("Step '%s' failed, notifying", step.Name)
			if !s.config.DryRun {
				if err := s.serverClient.Notify(ctx, pipeline, step, aud); err != nil {
					s.config.Logger.Errorf("failed to notify condition: %v", err)
				}

				labels := map[string]string{
					"service":        s.config.ServiceName,
					"component_name": req.ComponentName,
					"operation_name": req.OperationName,
					"pipeline_name":  pipeline.Name,
					"pipeline_id":    pipeline.Id,
				}
				_ = s.metrics.Incr(ctx, &types.CounterEntry{Name: types.NotifyCount, Labels: labels, Value: 1})
			}
		case protos.PipelineStepCondition_PIPELINE_STEP_CONDITION_ABORT:
			s.config.Logger.Debugf("Step '%s' failed, aborting further pipeline steps", step.Name)
			shouldContinue = false
		default:
			// Assume continue
			s.config.Logger.Debugf("Step '%s' failed, continuing to next step", step.Name)
		}
	}

	return shouldContinue
}

func (a *Audience) ToProto() *protos.Audience {
	return &protos.Audience{
		ServiceName:   a.ServiceName,
		ComponentName: a.ComponentName,
		OperationType: protos.OperationType(a.OperationType),
		OperationName: a.OperationName,
	}
}
