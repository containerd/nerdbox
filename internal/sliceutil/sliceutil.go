package sliceutil

func Filter[T any](slice []T, fn func(T) bool) []T {
	var filtered []T
	for _, v := range slice {
		if fn(v) {
			filtered = append(filtered, v)
		}
	}
	return filtered
}

func Map[T any, R any](slice []T, fn func(T) R) []R {
	mapped := make([]R, len(slice))
	for i, v := range slice {
		mapped[i] = fn(v)
	}
	return mapped
}
