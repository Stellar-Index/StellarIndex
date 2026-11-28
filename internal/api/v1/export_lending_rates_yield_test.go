package v1

import "time"

// SetLendingYieldReadBudgetForTest shortens how long /v1/lending/rates waits for a pool's yield inputs; the returned func
// restores it.
func SetLendingYieldReadBudgetForTest(d time.Duration) func() {
	prev := lendingYieldReadBudget
	lendingYieldReadBudget = d
	return func() { lendingYieldReadBudget = prev }
}
