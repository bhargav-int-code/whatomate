package queue

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/zerodha/logf"
)

const (
	// CampaignStatsChannel is the Redis pub/sub channel for campaign stats updates
	CampaignStatsChannel = "whatomate:campaign_stats"

	// ExportUpdatesChannel is the Redis pub/sub channel workers use to report
	// an export finishing, so the API can notify the requester
	ExportUpdatesChannel = "whatomate:export_updates"
)

// ExportUpdate reports that an export finished (completed or failed)
type ExportUpdate struct {
	ExportID       uuid.UUID `json:"export_id"`
	OrganizationID uuid.UUID `json:"organization_id"`
	UserID         uuid.UUID `json:"user_id"`
	Status         string    `json:"status"`
	FileName       string    `json:"file_name,omitempty"`
	RowCount       int64     `json:"row_count"`
	Error          string    `json:"error,omitempty"`
}

// CampaignStatsUpdate represents a campaign stats update message
type CampaignStatsUpdate struct {
	CampaignID     string                `json:"campaign_id"`
	OrganizationID uuid.UUID             `json:"organization_id"`
	Status         models.CampaignStatus `json:"status"`
	SentCount      int                   `json:"sent_count"`
	DeliveredCount int                   `json:"delivered_count"`
	ReadCount      int                   `json:"read_count"`
	FailedCount    int                   `json:"failed_count"`
}

// Publisher publishes messages to Redis pub/sub channels
type Publisher struct {
	client *redis.Client
	log    logf.Logger
}

// NewPublisher creates a new Redis publisher
func NewPublisher(client *redis.Client, log logf.Logger) *Publisher {
	return &Publisher{
		client: client,
		log:    log,
	}
}

// PublishCampaignStats publishes a campaign stats update
func (p *Publisher) PublishCampaignStats(ctx context.Context, update *CampaignStatsUpdate) error {
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}

	if err := p.client.Publish(ctx, CampaignStatsChannel, payload).Err(); err != nil {
		p.log.Error("Failed to publish campaign stats", "error", err, "campaign_id", update.CampaignID)
		return err
	}

	p.log.Debug("Published campaign stats update", "campaign_id", update.CampaignID, "status", update.Status)
	return nil
}

// PublishExportUpdate publishes an export's final state
func (p *Publisher) PublishExportUpdate(ctx context.Context, update *ExportUpdate) error {
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}

	if err := p.client.Publish(ctx, ExportUpdatesChannel, payload).Err(); err != nil {
		p.log.Error("Failed to publish export update", "error", err, "export_id", update.ExportID)
		return err
	}
	return nil
}

// Subscriber subscribes to Redis pub/sub channels. Use one Subscriber per
// channel.
type Subscriber struct {
	client *redis.Client
	log    logf.Logger
	pubsub *redis.PubSub
}

// NewSubscriber creates a new Redis subscriber
func NewSubscriber(client *redis.Client, log logf.Logger) *Subscriber {
	return &Subscriber{
		client: client,
		log:    log,
	}
}

// SubscribeCampaignStats subscribes to campaign stats updates
// The handler is called for each received update
func (s *Subscriber) SubscribeCampaignStats(ctx context.Context, handler func(update *CampaignStatsUpdate)) error {
	s.pubsub = s.client.Subscribe(ctx, CampaignStatsChannel)

	// Wait for subscription confirmation
	_, err := s.pubsub.Receive(ctx)
	if err != nil {
		return err
	}

	s.log.Info("Subscribed to campaign stats channel")

	// Start receiving messages
	ch := s.pubsub.Channel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				s.log.Info("Campaign stats subscriber shutting down")
				return
			case msg, ok := <-ch:
				if !ok {
					s.log.Info("Campaign stats channel closed")
					return
				}

				var update CampaignStatsUpdate
				if err := json.Unmarshal([]byte(msg.Payload), &update); err != nil {
					s.log.Error("Failed to unmarshal campaign stats update", "error", err)
					continue
				}

				handler(&update)
			}
		}
	}()

	return nil
}

// SubscribeExportUpdates subscribes to export updates. The handler is called
// for each received update until ctx is cancelled.
func (s *Subscriber) SubscribeExportUpdates(ctx context.Context, handler func(update *ExportUpdate)) error {
	s.pubsub = s.client.Subscribe(ctx, ExportUpdatesChannel)
	if _, err := s.pubsub.Receive(ctx); err != nil {
		return err
	}
	s.log.Info("Subscribed to export updates channel")

	ch := s.pubsub.Channel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var update ExportUpdate
				if err := json.Unmarshal([]byte(msg.Payload), &update); err != nil {
					s.log.Error("Failed to unmarshal export update", "error", err)
					continue
				}
				handler(&update)
			}
		}
	}()
	return nil
}

// Close closes the subscriber
func (s *Subscriber) Close() error {
	if s.pubsub != nil {
		return s.pubsub.Close()
	}
	return nil
}
