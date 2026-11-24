package testcause

// Diagnostics are fixed codes only; they never carry command or output data.
func addPassEvidenceMissing(clean Object, conditions bool, completed int, target, base, intact, failures bool, source string) {
	if clean == nil || clean["complete"] == true {
		return
	}
	missing := []string{}
	if !conditions {
		missing = append(missing, "conditions")
	}
	if completed <= 0 {
		missing = append(missing, "completion")
	}
	if !target {
		missing = append(missing, "target")
	}
	if !base {
		missing = append(missing, "base")
	}
	if !intact {
		missing = append(missing, "output")
	}
	if failures {
		missing = append(missing, "failures")
	}
	if source != "fixed-test" && source != "full-test" {
		missing = append(missing, "source")
	}
	if len(missing) == 0 {
		missing = append(missing, "format")
	}
	clean["passEvidenceMissing"] = missing
}
