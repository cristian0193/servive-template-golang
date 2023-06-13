package consumer

import (
	"encoding/json"
	"github.com/aws/aws-sdk-go/service/sqs"
	"go.uber.org/zap"
	"gorm.io/gorm/clause"
	"service-template-golang/clients/awssqs"
	"service-template-golang/database"
	"service-template-golang/domain"
	"service-template-golang/domain/entity"
	"sync"
	"time"
)

// SQSSource event stream representation to SQS.
type SQSSource struct {
	sqs         *awssqs.ClientSQS
	log         *zap.SugaredLogger
	maxMessages int
	closed      bool
	db          *database.ClientDB
	wg          sync.WaitGroup
}

// New return an event stream instance from SQS.
func New(sqsClient *awssqs.ClientSQS, logger *zap.SugaredLogger, maxMessages int, db *database.ClientDB) (*SQSSource, error) {
	return &SQSSource{
		sqs:         sqsClient,
		log:         logger,
		maxMessages: maxMessages,
		db:          db,
		wg:          sync.WaitGroup{},
	}, nil
}

// Consume opens a channel and sends entities created from SQS messages.
func (s *SQSSource) Consume() <-chan *domain.Event {
	out := make(chan *domain.Event, s.maxMessages)
	go func() {
		for {
			if s.closed {
				break
			}
			messages, err := s.sqs.GetMessages()
			if err != nil {
				s.log.Errorf("Error getting messages from SQS: %v", err)
				continue
			}
			if len(messages) == 0 {
				s.log.Debug("No messages found from SQS")
			}
			for _, msg := range messages {
				s.processMessage(msg, out)
			}
			s.wg.Wait()
		}
		close(out)
	}()

	return out
}

// processMessage read message in queue.
func (s *SQSSource) processMessage(msg *sqs.Message, out chan *domain.Event) {
	var records domain.Events
	err := json.Unmarshal([]byte(*msg.Body), &records)
	if err != nil {
		s.log.Errorf("Error processing message from SQS: %v", err)
		return
	}
	retry := "0"
	val, ok := msg.Attributes[sqs.MessageSystemAttributeNameApproximateReceiveCount]
	if ok {
		retry = *val
	}

	logger := s.log.With("retry", retry)
	logger.Infof("Start to process SQS event")

	eventDB := &entity.Events{
		ID:      *msg.MessageId,
		Message: records.Message,
		Date:    time.Now().String(),
	}

	if err = s.insertMessage(eventDB); err != nil {
		logger.Infof("error in insertMessage: %v", err)
	}

	event := &domain.Event{
		ID:            *msg.MessageId,
		Retry:         retry,
		Records:       records,
		OriginalEvent: msg,
		Log:           s.log,
	}
	s.wg.Add(1)
	logger.Infof("Event produced for ID = %s)", event.ID)
	out <- event
}

// insertMessage insert message in database.
func (s *SQSSource) insertMessage(events *entity.Events) error {
	r := s.db.DB.Clauses(clause.OnConflict{
		UpdateAll: true,
	}).Create(&events)
	if r.Error != nil {
		r.Rollback()
		return r.Error
	}
	return nil
}

// Processed notify that event of consolidate file was processed.
func (s *SQSSource) Processed(event *domain.Event) error {
	defer s.wg.Done()
	logger := event.Log

	if events, ok := event.OriginalEvent.(*sqs.Message); ok {
		if err := s.sqs.DeleteMessage(events); err != nil {
			logger.Errorf("error deleting of sqs message. %v", err)
			return err
		}
		logger.Infof("successful deleted sqs message")
		return nil
	}
	logger.Warnf("event isn't sqs message")
	return nil
}

// Close the event stream.
func (s *SQSSource) Close() error {
	s.closed = true
	s.wg.Wait()
	return nil
}
