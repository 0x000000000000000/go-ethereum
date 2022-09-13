package types

import "time"

func (tx *Transaction) Time() time.Time {
	return tx.time
}
