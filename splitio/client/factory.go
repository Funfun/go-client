// Package client contains implementations of the Split SDK client and the factory used
// to instantiate it.
package client

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/splitio/go-client/splitio"
	"github.com/splitio/go-client/splitio/conf"
	"github.com/splitio/go-client/splitio/engine"
	"github.com/splitio/go-client/splitio/engine/evaluator"
	impressionlistener "github.com/splitio/go-client/splitio/impressionListener"
	config "github.com/splitio/go-split-commons/conf"
	"github.com/splitio/go-split-commons/dtos"
	"github.com/splitio/go-split-commons/service"
	"github.com/splitio/go-split-commons/service/api"
	"github.com/splitio/go-split-commons/service/local"
	"github.com/splitio/go-split-commons/storage"
	"github.com/splitio/go-split-commons/storage/mutexmap"
	"github.com/splitio/go-split-commons/storage/mutexqueue"
	"github.com/splitio/go-split-commons/storage/redis"
	synchronizer "github.com/splitio/go-split-commons/sync"
	"github.com/splitio/go-toolkit/logging"
)

const (
	sdkStatusDestroyed = iota
	sdkStatusInitializing
	sdkStatusReady

	sdkInitializationFailed = -1
)

type sdkStorages struct {
	splits      storage.SplitStorage
	segments    storage.SegmentStorage
	impressions storage.ImpressionStorage
	events      storage.EventsStorage
	telemetry   storage.MetricsStorage
}

// SplitFactory struct is responsible for instantiating and storing instances of client and manager.
type SplitFactory struct {
	metadata              dtos.Metadata
	storages              sdkStorages
	apikey                string
	status                atomic.Value
	readinessSubscriptors map[int]chan int
	operationMode         string
	mutex                 sync.Mutex
	cfg                   *conf.SplitSdkConfig
	impressionListener    *impressionlistener.WrapperImpressionListener
	logger                logging.LoggerInterface
	syncManager           *synchronizer.SynchronizerManager
}

// Client returns the split client instantiated by the factory
func (f *SplitFactory) Client() *SplitClient {
	return &SplitClient{
		logger:      f.logger,
		evaluator:   evaluator.NewEvaluator(f.storages.splits, f.storages.segments, engine.NewEngine(f.logger), f.logger),
		impressions: f.storages.impressions,
		metrics:     f.storages.telemetry,
		events:      f.storages.events,
		validator: inputValidation{
			logger:       f.logger,
			splitStorage: f.storages.splits,
		},
		factory:            f,
		impressionListener: f.impressionListener,
	}
}

// Manager returns the split manager instantiated by the factory
func (f *SplitFactory) Manager() *SplitManager {
	return &SplitManager{
		splitStorage: f.storages.splits,
		validator:    inputValidation{logger: f.logger},
		logger:       f.logger,
		factory:      f,
	}
}

// IsDestroyed returns true if tbe client has been destroyed
func (f *SplitFactory) IsDestroyed() bool {
	return f.status.Load() == sdkStatusDestroyed
}

// IsReady returns true if the factory is ready
func (f *SplitFactory) IsReady() bool {
	return f.status.Load() == sdkStatusReady
}

// initializates task for localhost mode
func (f *SplitFactory) initializationLocalhost(readyChannel chan string) {
	f.syncManager.Start()

	<-readyChannel
	f.broadcastReadiness(sdkStatusReady)
}

// initializates tasks for in-memory mode
func (f *SplitFactory) initializationInMemory(readyChannel chan string) {
	f.syncManager.Start()
	msg := <-readyChannel
	switch msg {
	case "READY":
		// Broadcast ready status for SDK
		f.broadcastReadiness(sdkStatusReady)
	default:
		f.broadcastReadiness(sdkInitializationFailed)
	}
}

// broadcastReadiness broadcasts message to all the subscriptors
func (f *SplitFactory) broadcastReadiness(status int) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.status.Load() == sdkStatusInitializing && status == sdkStatusReady {
		f.status.Store(sdkStatusReady)
	}
	for _, subscriptor := range f.readinessSubscriptors {
		subscriptor <- status
	}
}

// subscribes listener
func (f *SplitFactory) subscribe(name int, subscriptor chan int) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.readinessSubscriptors[name] = subscriptor
}

// removes a particular subscriptor from the list
func (f *SplitFactory) unsubscribe(name int, subscriptor chan int) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	_, ok := f.readinessSubscriptors[name]
	if ok {
		delete(f.readinessSubscriptors, name)
	}
}

// BlockUntilReady blocks client or manager until the SDK is ready, error occurs or times out
func (f *SplitFactory) BlockUntilReady(timer int) error {
	if f.IsReady() {
		return nil
	}
	if timer <= 0 {
		return errors.New("SDK Initialization: timer must be positive number")
	}
	if f.IsDestroyed() {
		return errors.New("SDK Initialization: Client is destroyed")
	}
	block := make(chan int, 1)

	f.mutex.Lock()
	subscriptorName := len(f.readinessSubscriptors)
	f.mutex.Unlock()

	defer func() {
		// Unsubscription will happen only if a block channel has been created
		if block != nil {
			f.unsubscribe(subscriptorName, block)
			close(block)
		}
	}()

	f.subscribe(subscriptorName, block)

	select {
	case status := <-block:
		switch status {
		case sdkStatusReady:
			break
		case sdkInitializationFailed:
			return errors.New("SDK Initialization failed")
		}
	case <-time.After(time.Second * time.Duration(timer)):
		return fmt.Errorf("SDK Initialization: time of %d exceeded", timer)
	}

	return nil
}

// Destroy stops all async tasks and clears all storages
func (f *SplitFactory) Destroy() {
	if !f.IsDestroyed() {
		removeInstanceFromTracker(f.apikey)
	}
	f.status.Store(sdkStatusDestroyed)

	if f.cfg.OperationMode == "redis-consumer" {
		return
	}

	f.syncManager.Stop()
}

// setupLogger sets up the logger according to the parameters submitted by the sdk user
func setupLogger(cfg *conf.SplitSdkConfig) logging.LoggerInterface {
	var logger logging.LoggerInterface
	if cfg.Logger != nil {
		// If a custom logger is supplied, use it.
		logger = cfg.Logger
	} else {
		logger = logging.NewLogger(&cfg.LoggerConfig)
	}
	return logger
}

func setupInMemoryFactory(
	apikey string,
	cfg *conf.SplitSdkConfig,
	logger logging.LoggerInterface,
	metadata dtos.Metadata,
) (*SplitFactory, error) {
	advanced := &config.AdvancedConfig{
		EventsQueueSize:      cfg.Advanced.EventsQueueSize,
		EventsBulkSize:       cfg.Advanced.EventsBulkSize,
		EventsURL:            cfg.Advanced.EventsURL,
		HTTPTimeout:          cfg.Advanced.HTTPTimeout,
		ImpressionsBulkSize:  cfg.Advanced.ImpressionsBulkSize,
		ImpressionsQueueSize: cfg.Advanced.ImpressionsQueueSize,
		SdkURL:               cfg.Advanced.SdkURL,
		SegmentQueueSize:     cfg.Advanced.SegmentQueueSize,
		SegmentWorkers:       cfg.Advanced.SegmentWorkers,
	}

	err := api.ValidateApikey(apikey, *advanced)
	if err != nil {
		return nil, err
	}

	inMememoryFullQueue := make(chan string, 2) // Size 2: So that it's able to accept one event from each resource simultaneously.

	storages := sdkStorages{
		splits:      mutexmap.NewMMSplitStorage(),
		segments:    mutexmap.NewMMSegmentStorage(),
		impressions: mutexqueue.NewMQImpressionsStorage(cfg.Advanced.ImpressionsQueueSize, inMememoryFullQueue, logger),
		telemetry:   mutexmap.NewMMMetricsStorage(),
		events:      mutexqueue.NewMQEventsStorage(cfg.Advanced.EventsQueueSize, inMememoryFullQueue, logger),
	}

	readyChannel := make(chan string, 1)

	splitAPI := service.NewSplitAPI(apikey, splitio.Version, advanced, logger, metadata)

	syncImpl := synchronizer.NewSynchronizer(
		config.TaskPeriods{
			CounterSync:    cfg.TaskPeriods.CounterSync,
			EventsSync:     cfg.TaskPeriods.EventsSync,
			GaugeSync:      cfg.TaskPeriods.GaugeSync,
			ImpressionSync: cfg.TaskPeriods.ImpressionSync,
			LatencySync:    cfg.TaskPeriods.LatencySync,
			SegmentSync:    cfg.TaskPeriods.SegmentSync,
			SplitSync:      cfg.TaskPeriods.SplitSync,
		},
		*advanced,
		splitAPI,
		storages.splits,
		storages.segments,
		storages.telemetry,
		storages.impressions,
		storages.events,
		logger,
		inMememoryFullQueue,
	)

	syncManager := synchronizer.NewSynchronizerManager(
		syncImpl,
		logger,
		readyChannel,
	)

	splitFactory := SplitFactory{
		apikey:                apikey,
		cfg:                   cfg,
		metadata:              metadata,
		logger:                logger,
		operationMode:         "inmemory-standalone",
		storages:              storages,
		readinessSubscriptors: make(map[int]chan int),
		syncManager:           syncManager,
	}
	splitFactory.status.Store(sdkStatusInitializing)

	go splitFactory.initializationInMemory(readyChannel)

	return &splitFactory, nil
}

func setupRedisFactory(apikey string, cfg *conf.SplitSdkConfig, logger logging.LoggerInterface, metadata dtos.Metadata) (*SplitFactory, error) {
	redisClient, err := redis.NewRedisClient(&cfg.Redis, logger)
	if err != nil {
		logger.Error("Failed to instantiate redis client.")
		return nil, err
	}

	storages := sdkStorages{
		splits:      redis.NewSplitStorage(redisClient, logger),
		segments:    redis.NewSegmentStorage(redisClient, logger),
		impressions: redis.NewImpressionStorage(redisClient, metadata, logger),
		telemetry:   redis.NewMetricsStorage(redisClient, metadata, logger),
		events:      redis.NewEventsStorage(redisClient, metadata, logger),
	}

	factory := &SplitFactory{
		apikey:                apikey,
		cfg:                   cfg,
		metadata:              metadata,
		logger:                logger,
		operationMode:         "redis-consumer",
		storages:              storages,
		readinessSubscriptors: make(map[int]chan int),
	}
	factory.status.Store(sdkStatusReady)
	return factory, nil
}

func setupLocalhostFactory(
	apikey string,
	cfg *conf.SplitSdkConfig,
	logger logging.LoggerInterface,
	metadata dtos.Metadata,
) (*SplitFactory, error) {
	splitStorage := mutexmap.NewMMSplitStorage()
	splitPeriod := cfg.TaskPeriods.SplitSync
	readyChannel := make(chan string, 1)

	syncManager := synchronizer.NewSynchronizerManager(
		synchronizer.NewLocalSynchronizer(
			splitPeriod,
			&service.SplitAPI{
				SplitFetcher: local.NewFileSplitFetcher(cfg.SplitFile, logger),
			},
			splitStorage,
			logger,
		),
		logger,
		readyChannel,
	)

	splitFactory := &SplitFactory{
		apikey:   apikey,
		cfg:      cfg,
		metadata: metadata,
		logger:   logger,
		storages: sdkStorages{
			splits:      splitStorage,
			impressions: mutexqueue.NewMQImpressionsStorage(cfg.Advanced.ImpressionsQueueSize, make(chan string, 1), logger),
			telemetry:   mutexmap.NewMMMetricsStorage(),
			events:      mutexqueue.NewMQEventsStorage(cfg.Advanced.EventsQueueSize, make(chan string, 1), logger),
			segments:    mutexmap.NewMMSegmentStorage(),
		},
		readinessSubscriptors: make(map[int]chan int),
		syncManager:           syncManager,
	}
	splitFactory.status.Store(sdkStatusInitializing)

	// Call fetching tasks as goroutine
	go splitFactory.initializationLocalhost(readyChannel)

	return splitFactory, nil
}

// newFactory instantiates a new SplitFactory object. Accepts a SplitSdkConfig struct as an argument,
// which will be used to instantiate both the client and the manager
func newFactory(apikey string, cfg *conf.SplitSdkConfig, logger logging.LoggerInterface) (*SplitFactory, error) {
	metadata := dtos.Metadata{
		SDKVersion:  "go-" + splitio.Version,
		MachineIP:   cfg.IPAddress,
		MachineName: cfg.InstanceName,
	}

	var splitFactory *SplitFactory
	var err error

	switch cfg.OperationMode {
	case "inmemory-standalone":
		splitFactory, err = setupInMemoryFactory(apikey, cfg, logger, metadata)
	case "redis-consumer":
		splitFactory, err = setupRedisFactory(apikey, cfg, logger, metadata)
	case "localhost":
		splitFactory, err = setupLocalhostFactory(apikey, cfg, logger, metadata)
	default:
		err = fmt.Errorf("Invalid operation mode \"%s\"", cfg.OperationMode)
	}

	if err != nil {
		return nil, err
	}

	if cfg.Advanced.ImpressionListener != nil {
		splitFactory.impressionListener = impressionlistener.NewImpressionListenerWrapper(
			cfg.Advanced.ImpressionListener,
			metadata,
		)
	}

	return splitFactory, nil
}
