package domain

// Status é o estado de uma transação financeira.
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

// IsTerminal informa se o estado não admite novas transições.
func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// CanTransitionTo valida a máquina de estados. Reagendar uma pendência não é transição:
// PENDING_REFERENCE continua PENDING_REFERENCE e só muda tentativas e próximo instante.
func (s Status) CanTransitionTo(to Status) bool {
	switch s {
	case StatusPending:
		return to == StatusPendingReference || to.IsTerminal()
	case StatusPendingReference:
		return to.IsTerminal()
	}
	return false
}
