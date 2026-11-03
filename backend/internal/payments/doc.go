// Package payments drives the Payment state machine (created, authorized,
// captured, settled, failed, refunded, disputed) for a single attempt to pay
// an Order via one payment method, delegating the actual authorize/capture
// calls to the method adapter in internal/methods.
package payments
