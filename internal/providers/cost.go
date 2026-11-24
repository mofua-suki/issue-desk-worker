package providers

import "math"

// ValidCostUSD reports whether a measured cost can be recorded as JSON.
func ValidCostUSD(value *float64) bool {
	return value != nil && *value >= 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}
