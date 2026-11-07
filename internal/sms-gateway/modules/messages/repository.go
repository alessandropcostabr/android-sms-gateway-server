package messages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/android-sms-gateway/server/pkg/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxPendingBatch = 100

type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{
		db: db,
	}
}

func (r *Repository) list(filter SelectFilter, options SelectOptions) ([]messageModel, int64, error) {
	query := r.db.Model((*messageModel)(nil))

	// Apply date range filter
	if !filter.StartDate.IsZero() {
		query = query.Where("messages.created_at >= ?", filter.StartDate)
	}
	if !filter.EndDate.IsZero() {
		query = query.Where("messages.created_at < ?", filter.EndDate)
	}

	// Apply ID filter
	if filter.ExtID != "" {
		query = query.Where("messages.ext_id = ?", filter.ExtID)
	}

	// Apply user filter
	if filter.UserID != "" {
		query = query.Where("messages.user_id = ?", filter.UserID)
	}

	// Apply state filter
	if len(filter.State) > 0 {
		query = query.Where("messages.state IN ?", filter.State)
	}

	// Apply device filter
	if filter.DeviceID != "" {
		query = query.Where("messages.device_id = ?", filter.DeviceID)
	}

	// Get total count
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// Apply pagination
	if options.Limit > 0 {
		query = query.Limit(options.Limit)
	}
	if options.Offset > 0 {
		query = query.Offset(options.Offset)
	}

	// Apply ordering
	switch {
	case options.SortField == SortFieldCreatedAtAsc:
		query = query.Order("messages.created_at ASC, messages.id ASC")
	case options.SortField == SortFieldCreatedAtDesc:
		query = query.Order("messages.created_at DESC, messages.id DESC")
	case options.OrderBy == MessagesOrderFIFO:
		query = query.Order("messages.schedule_at ASC, messages.priority DESC, messages.id ASC")
	default:
		query = query.Order("messages.schedule_at ASC, messages.priority DESC, messages.id DESC")
	}

	// Preload related data
	if options.WithRecipients {
		query = query.Preload("Recipients")
	}
	if filter.UserID == "" && options.WithDevice {
		query = query.Joins("Device")
	}
	if options.WithStates {
		query = query.Preload("States")
	}

	// Apply content filter
	if !options.WithContent {
		query = query.Omit("Content")
	}

	messages := make([]messageModel, 0, min(options.Limit, int(total)))
	if err := query.Find(&messages).Error; err != nil {
		return nil, 0, fmt.Errorf("failed to select messages: %w", err)
	}

	return messages, total, nil
}

func (r *Repository) listPending(deviceID string, order Order) ([]messageModel, error) {
	messages, _, err := r.list(
		*new(SelectFilter).WithDeviceID(deviceID).WithState(ProcessingStatePending).WithState(ProcessingStateCancelling),
		*new(SelectOptions).IncludeContent().IncludeRecipients().WithLimit(maxPendingBatch).WithOrderBy(order),
	)

	return messages, err
}

func (r *Repository) get(filter SelectFilter, options SelectOptions) (messageModel, error) {
	messages, _, err := r.list(filter, options)
	if err != nil {
		return messageModel{}, fmt.Errorf("failed to get message: %w", err)
	}

	if len(messages) == 0 {
		return messageModel{}, ErrMessageNotFound
	}

	if len(messages) > 1 {
		return messageModel{}, ErrMultipleMessagesFound
	}

	return messages[0], nil
}

func (r *Repository) Insert(message *messageModel) error {
	err := r.db.Transaction(func(tx *gorm.DB) error {
		registry := messageIDRegistryModel{
			UserID:          message.UserID,
			MessageIDSHA256: messageIDRegistryHash(message.UserID, message.ExtID),
			State:           messageIDRegistryStateActive,
		}
		if err := tx.Create(&registry).Error; err != nil {
			if !errors.Is(err, gorm.ErrDuplicatedKey) && !mysql.IsDuplicateKeyViolation(err) {
				return err
			}

			var existing messageIDRegistryModel
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&existing,
				"user_id = ? AND message_id_sha256 = ?",
				registry.UserID,
				registry.MessageIDSHA256,
			).Error; err != nil {
				return err
			}
			if existing.State == messageIDRegistryStatePurged {
				return ErrMessagePurged
			}
			return ErrMessageAlreadyExists
		}

		return tx.Omit("Device").Create(message).Error
	})
	if err != nil {
		if errors.Is(err, ErrMessagePurged) {
			return ErrMessagePurged
		}
		if errors.Is(err, ErrMessageAlreadyExists) || errors.Is(err, gorm.ErrDuplicatedKey) || mysql.IsDuplicateKeyViolation(err) {
			return ErrMessageAlreadyExists
		}
		return fmt.Errorf("failed to insert message: %w", err)
	}

	return r.loadCreatedAt(message)
}

func messageIDRegistryHash(userID string, id string) string {
	digest := sha256.Sum256([]byte(userID + "\n" + id))
	return hex.EncodeToString(digest[:])
}

func (r *Repository) PurgeMessage(userID string, id string) error {
	now := time.Now().UTC()
	registry := messageIDRegistryModel{
		UserID:          userID,
		MessageIDSHA256: messageIDRegistryHash(userID, id),
		State:           messageIDRegistryStatePurged,
		PurgedAt:        &now,
	}

	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}, {Name: "message_id_sha256"}},
			DoUpdates: clause.Assignments(map[string]any{
				"state":     messageIDRegistryStatePurged,
				"purged_at": now,
			}),
		}).Create(&registry).Error; err != nil {
			return err
		}

		return tx.Where("user_id = ? AND ext_id = ?", userID, id).
			Delete(new(messageModel)).Error
	})
	if err != nil {
		return fmt.Errorf("failed to purge message: %w", err)
	}

	return nil
}

func (r *Repository) IsMessagePurged(userID string, id string) (bool, error) {
	var count int64
	err := r.db.Model(new(messageIDRegistryModel)).
		Where("user_id = ? AND message_id_sha256 = ? AND state = ?",
			userID,
			messageIDRegistryHash(userID, id),
			messageIDRegistryStatePurged,
		).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("failed to check message purge registry: %w", err)
	}

	return count > 0, nil
}

// loadCreatedAt reads the creation time assigned by the database back into the message.
//
// `created_at` is read-only for GORM (`gorm:"->"`), so the column is excluded from the INSERT and
// is filled in by its `DEFAULT CURRENT_TIMESTAMP(3)`. GORM reads such DB-generated columns back
// only through RETURNING, which the MySQL driver enables for MariaDB >= 10.5 only - on MySQL the
// in-memory value stays zero. Reading the column keeps the state returned by Enqueue identical to
// the one every later read observes.
func (r *Repository) loadCreatedAt(message *messageModel) error {
	if !message.CreatedAt.IsZero() {
		return nil
	}

	if err := r.db.
		Model(message).
		Select("created_at").
		Where("id = ?", message.ID).
		Take(message).Error; err != nil {
		return fmt.Errorf("failed to read created_at of inserted message: %w", err)
	}

	return nil
}

func (r *Repository) UpdateState(message *messageModel) error {
	err := r.db.Transaction(func(tx *gorm.DB) error {
		var registry messageIDRegistryModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(
			&registry,
			"user_id = ? AND message_id_sha256 = ?",
			message.UserID,
			messageIDRegistryHash(message.UserID, message.ExtID),
		).Error; err != nil {
			return err
		}
		if registry.State == messageIDRegistryStatePurged {
			return ErrMessageNotFound
		}

		if err := tx.Model(message).Select("State").Updates(message).Error; err != nil {
			return err
		}

		for _, v := range message.States {
			v.MessageID = message.ID
			if err := tx.Model(&v).Clauses(clause.OnConflict{
				DoNothing: true,
			}).Create(&v).Error; err != nil {
				return err
			}
		}

		for _, v := range message.Recipients {
			if err := tx.Model((*messageRecipientModel)(nil)).
				Where("message_id = ? AND phone_number = ?", message.ID, v.PhoneNumber).
				Select("state", "error").
				Updates(map[string]any{"state": v.State, "error": v.Error}).Error; err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to update message state: %w", err)
	}

	return nil
}

func (r *Repository) HashProcessed(ctx context.Context, ids []uint64) (int64, error) {
	rawSQL := "UPDATE `messages` `m`, `message_recipients` `r`\n" +
		"SET `m`.`is_hashed` = true, `m`.`content` = SHA2(COALESCE(JSON_VALUE(`content`, '$.text'), JSON_VALUE(`content`, '$.data'), JSON_VALUE(`content`, '$.subject'), JSON_EXTRACT(`content`, '$.attachments')), 256), `r`.`phone_number` = LEFT(SHA2(phone_number, 256), 16)\n" +
		"WHERE `m`.`id` = `r`.`message_id` AND `m`.`is_hashed` = false AND `m`.`is_encrypted` = false AND `m`.`state` NOT IN ('Pending', 'Cancelling')"
	params := []any{}
	if len(ids) > 0 {
		rawSQL += " AND `m`.`id` IN (?)"
		params = append(params, ids)
	}

	res := r.db.WithContext(ctx).
		Exec(rawSQL, params...)
	if res.Error != nil {
		return 0, fmt.Errorf("sql error: %w", res.Error)
	}

	return res.RowsAffected, nil
}

func (r *Repository) CancelMessage(userID string, id string) error {
	res := r.db.Model((*messageModel)(nil)).
		Where("ext_id = ? AND state = ? AND user_id = ?", id, ProcessingStatePending, userID).
		Update("state", ProcessingStateCancelling)
	if res.Error != nil {
		return fmt.Errorf("failed to cancel message: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrMessageNotPending
	}
	return nil
}

func (r *Repository) Cleanup(ctx context.Context, until time.Time) (int64, error) {
	var deleted int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.
			Where("state NOT IN ?", []ProcessingState{ProcessingStatePending, ProcessingStateCancelling}).
			Where("created_at < ?", until).
			Delete(new(messageModel))
		deleted = res.RowsAffected
		if res.Error != nil {
			return res.Error
		}

		// Active IDs follow normal message retention; purged IDs remain reserved permanently.
		return tx.Exec(`
			DELETE r
			FROM message_id_registry AS r
			WHERE r.state = ?
				AND NOT EXISTS (
					SELECT 1
					FROM messages AS m
					WHERE m.user_id = r.user_id
						AND SHA2(CONCAT(m.user_id, CHAR(10), m.ext_id), 256) = r.message_id_sha256
				)`,
			messageIDRegistryStateActive,
		).Error
	})

	return deleted, err
}

func (r *Repository) CountPending(ctx context.Context, deviceID string) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).
		Model((*messageModel)(nil)).
		Where("device_id = ?", deviceID).
		Where("state = ?", ProcessingStatePending).
		Where("schedule_at IS NULL OR schedule_at <= ?", time.Now()).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("failed to count pending messages: %w", err)
	}
	return count, nil
}

func (r *Repository) GetOldestPendingTime(ctx context.Context, deviceID string) (*time.Time, error) {
	var msg messageModel
	err := r.db.WithContext(ctx).
		Model((*messageModel)(nil)).
		Select("created_at").
		Where("device_id = ?", deviceID).
		Where("state = ?", ProcessingStatePending).
		Where("schedule_at IS NULL OR schedule_at <= ?", time.Now()).
		Order("id ASC").
		First(&msg).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil //nolint:nilnil // special meaning
		}
		return nil, fmt.Errorf("failed to get oldest pending message: %w", err)
	}
	return &msg.CreatedAt, nil
}

func (r *Repository) GetStatesInTimeWindow(
	ctx context.Context,
	deviceID string,
	since time.Time,
	limit int,
) ([]ProcessingState, error) {
	var states []ProcessingState
	err := r.db.WithContext(ctx).
		Model((*messageModel)(nil)).
		Select("state").
		Where("device_id = ? AND created_at >= ?", deviceID, since).
		Order("id DESC").
		Limit(limit).
		Pluck("state", &states).Error
	if err != nil {
		return nil, fmt.Errorf("failed to get failed message states in time window: %w", err)
	}
	return states, nil
}
