package main

// The server owns operation availability and authorization. Restrict the path
// grammar here, not a second feature registry that drifts from the relay.
func validAutomationOperation(operation string) bool {
	if len(operation) < 3 || len(operation) > 80 {
		return false
	}
	separator := -1
	for i, char := range operation {
		if char == '.' {
			if separator != -1 || i == 0 || i == len(operation)-1 {
				return false
			}
			separator = i
			continue
		}
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' {
			continue
		}
		return false
	}
	return separator != -1
}
