// Package webhooks dispatches signed (HMAC-SHA256), at-least-once webhook
// deliveries to merchant endpoints in reaction to domain events, with
// exponential backoff retry and manual redelivery support.
package webhooks
