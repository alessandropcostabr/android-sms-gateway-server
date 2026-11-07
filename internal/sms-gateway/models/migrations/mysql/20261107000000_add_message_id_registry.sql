-- +goose Up
-- +goose StatementBegin
CREATE TABLE `message_id_registry` (
    `user_id` varchar(32) NOT NULL,
    `message_id_sha256` char(64) NOT NULL,
    `state` enum('active','purged') NOT NULL DEFAULT 'active',
    `active_references` int unsigned NOT NULL DEFAULT 1,
    `created_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `purged_at` datetime(3) NULL,
    PRIMARY KEY (`user_id`, `message_id_sha256`),
    INDEX `idx_message_id_registry_state_references` (`state`, `active_references`),
    CONSTRAINT `fk_message_id_registry_user`
        FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO `message_id_registry` (
    `user_id`,
    `message_id_sha256`,
    `state`,
    `active_references`,
    `created_at`
)
SELECT
    `user_id`,
    SHA2(CONCAT(`user_id`, CHAR(10), `ext_id`), 256),
    'active',
    COUNT(*),
    MIN(`created_at`)
FROM `messages`
GROUP BY `user_id`, SHA2(CONCAT(`user_id`, CHAR(10), `ext_id`), 256);
-- +goose StatementEnd
---
-- +goose Down
-- +goose StatementBegin
DROP TABLE `message_id_registry`;
-- +goose StatementEnd
