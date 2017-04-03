package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/cloudfoundry/sonde-go/events"
)

const (
	// TopicAppLogTmpl is Kafka topic name template for LogMessage
	TopicAppLogTmpl = "app-log-%s"

	// TopicCFMetrics is Kafka topic name for ValueMetric
	TopicCFMetric = "cf-metrics"
)

const (
	// Default topic name for each event
	DefaultValueMetricTopic = "value-metric"
	DefaultLogMessageTopic  = "log-message"

	DefaultKafkaRepartitionMax = 5
	DefaultKafkaRetryMax       = 1
	DefaultKafkaRetryBackoff   = 100 * time.Millisecond

	DefaultChannelBufferSize  = 512 // Sarama default is 256
	DefaultSubInputBufferSize = 1024
)

func NewKafkaProducer(logger *log.Logger, stats *Stats, config *Config) (NozzleProducer, error) {
	// Setup kafka async producer (We must use sync producer)
	// TODO (tcnksm): Enable to configure more properties.
	producerConfig := sarama.NewConfig()

	producerConfig.Producer.Partitioner = sarama.NewRoundRobinPartitioner
	producerConfig.Producer.Return.Successes = true
	producerConfig.Producer.RequiredAcks = sarama.WaitForAll

	// This is the default, but Errors are required for repartitioning
	producerConfig.Producer.Return.Errors = true

	producerConfig.Producer.Retry.Max = DefaultKafkaRetryMax
	if config.Kafka.RetryMax != 0 {
		producerConfig.Producer.Retry.Max = config.Kafka.RetryMax
	}

	producerConfig.Producer.Retry.Backoff = DefaultKafkaRetryBackoff
	if config.Kafka.RetryBackoff != 0 {
		backoff := time.Duration(config.Kafka.RetryBackoff) * time.Millisecond
		producerConfig.Producer.Retry.Backoff = backoff
	}

	producerConfig.ChannelBufferSize = DefaultChannelBufferSize

	brokers := config.Kafka.Brokers
	if len(brokers) < 1 {
		return nil, fmt.Errorf("brokers are not provided")
	}

	asyncProducer, err := sarama.NewAsyncProducer(brokers, producerConfig)
	if err != nil {
		return nil, err
	}

	kafkaTopic := config.Kafka.Topic
	if kafkaTopic.LogMessage == "" {
		kafkaTopic.LogMessage = DefaultLogMessageTopic
	}

	if kafkaTopic.ValueMetric == "" {
		kafkaTopic.ValueMetric = DefaultValueMetricTopic
	}

	repartitionMax := DefaultKafkaRepartitionMax
	if config.Kafka.RepartitionMax != 0 {
		repartitionMax = config.Kafka.RepartitionMax
	}

	subInputBuffer := DefaultSubInputBufferSize
	subInputCh := make(chan *sarama.ProducerMessage, subInputBuffer)

	return &KafkaProducer{
		AsyncProducer:      asyncProducer,
		Logger:             logger,
		Stats:              stats,
		logMessageTopic:    kafkaTopic.LogMessage,
		logMessageTopicFmt: kafkaTopic.LogMessageFmt,
		valueMetricTopic:   kafkaTopic.ValueMetric,
		repartitionMax:     repartitionMax,
		subInputCh:         subInputCh,
		errors:             make(chan *sarama.ProducerError),
	}, nil
}

// KafkaProducer implements NozzleProducer interfaces
type KafkaProducer struct {
	sarama.AsyncProducer

	repartitionMax int
	errors         chan *sarama.ProducerError

	// SubInputCh is buffer for re-partitioning
	subInputCh chan *sarama.ProducerMessage

	logMessageTopic    string
	logMessageTopicFmt string

	valueMetricTopic string

	Logger *log.Logger
	Stats  *Stats

	once sync.Once
}

// metadata is metadata which will be injected to ProducerMessage.Metadata.
// This is used only when publish is failed and re-partitioning by ourself.
type metadata struct {
	// retires is the number of re-partitioning
	retries int
}

// init sets default logger
func (kp *KafkaProducer) init() {
	if kp.Logger == nil {
		kp.Logger = defaultLogger
	}
}

func (kp *KafkaProducer) LogMessageTopic(appID string) string {
	if kp.logMessageTopicFmt != "" {
		return fmt.Sprintf(kp.logMessageTopicFmt, appID)
	}

	return kp.logMessageTopic
}

func (kp *KafkaProducer) ValueMetricTopic() string {
	return kp.valueMetricTopic
}

func (kp *KafkaProducer) Errors() <-chan *sarama.ProducerError {
	return kp.errors
}

// Produce produces event to kafka
func (kp *KafkaProducer) Produce(ctx context.Context, eventCh <-chan *events.Envelope) {
	kp.once.Do(kp.init)

	kp.Logger.Printf("[INFO] Start to watching producer error for re-partition")
	go func() {
		for producerErr := range kp.AsyncProducer.Errors() {
			// Instead of giving up, try to resubmit the message so that it can end up
			// on a different partition (we don't care about order of message)
			// This is a workaround for https://github.com/Shopify/sarama/issues/514
			meta, _ := producerErr.Msg.Metadata.(metadata)
			kp.Logger.Printf("[ERROR] Producer error %+v", producerErr)

			if meta.retries >= kp.repartitionMax {
				kp.errors <- producerErr
				continue
			}

			// NOTE: We need to re-create Message because original message
			// which producer.Error stores internal state (unexported field)
			// and it effect partitioning.
			originalMsg := producerErr.Msg
			msg := &sarama.ProducerMessage{
				Topic: originalMsg.Topic,
				Value: originalMsg.Value,

				// Update retry count
				Metadata: metadata{
					retries: meta.retries + 1,
				},
			}

			// If sarama buffer is full, then input it to nozzle side buffer
			// (subInput) and retry to produce it later. When subInput is
			// full, we drop message.
			//
			// TODO(tcnksm): Monitor subInput buffer.
			select {
			case kp.Input() <- msg:
				kp.Logger.Printf("[DEBUG] Repartitioning")
			default:
				select {
				case kp.subInputCh <- msg:
					kp.Stats.Inc(SubInputBuffer)
				default:
					// If subInput is full, then drop message.....
					kp.errors <- producerErr
				}
			}
		}
	}()

	kp.Logger.Printf("[INFO] Start to sub input (buffer for sarama input)")
	go func() {
		for msg := range kp.subInputCh {
			kp.Input() <- msg
			kp.Logger.Printf("[DEBUG] Repartitioning (from subInput)")
			kp.Stats.Dec(SubInputBuffer)
		}
	}()

	kp.Logger.Printf("[INFO] Start loop to watch events")
	for {
		select {
		case <-ctx.Done():
			// Stop process immediately
			kp.Logger.Printf("[INFO] Stop kafka producer")
			return

		case event, ok := <-eventCh:
			if !ok {
				kp.Logger.Printf("[ERROR] Nozzle consumer eventCh is closed")
				return
			}

			kp.input(event)
		}
	}
}

func (kp *KafkaProducer) input(event *events.Envelope) {
	switch event.GetEventType() {
	case events.Envelope_HttpStart:
		// Do nothing
	case events.Envelope_HttpStartStop:
		// Do nothing
	case events.Envelope_HttpStop:
		// Do nothing
	case events.Envelope_LogMessage:
		kp.Stats.Inc(Consume)
		appID := event.GetLogMessage().GetAppId()
		kp.Input() <- &sarama.ProducerMessage{
			Topic:    kp.LogMessageTopic(appID),
			Value:    &JsonEncoder{event: event},
			Metadata: metadata{retries: 0},
		}
	case events.Envelope_ValueMetric:
		kp.Stats.Inc(Consume)
		kp.Input() <- &sarama.ProducerMessage{
			Topic:    kp.ValueMetricTopic(),
			Value:    &JsonEncoder{event: event},
			Metadata: metadata{retries: 0},
		}
	case events.Envelope_CounterEvent:
		// Do nothing
	case events.Envelope_Error:
		// Do nothing
	case events.Envelope_ContainerMetric:
		// Do nothing
	}
}
