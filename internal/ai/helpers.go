package ai

// orDefaultMax prefers the provider-configured max tokens, falling back to the
// per-request value when the provider leaves it unset.
func orDefaultMax(providerMax, reqMax int) int {
	if providerMax > 0 {
		return providerMax
	}
	return reqMax
}

// pickTemp prefers the provider-configured temperature when set (>0), otherwise
// the per-request value.
func pickTemp(providerTemp, reqTemp float64) float64 {
	if providerTemp > 0 {
		return providerTemp
	}
	return reqTemp
}
