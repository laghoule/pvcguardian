// SPDX-License-Identifier: GPL-3.0-only
package k8s

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

// parseSelectorAnnotation parses the value of pvcguardian.io/selector into
// matchLabels. The supported syntax is a comma-separated list of key=value
// pairs (equality only), e.g. "app.kubernetes.io/part-of=coroot".
func parseSelectorAnnotation(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	matchLabels := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("invalid selector entry %q: expected key=value", pair)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "" {
			return nil, fmt.Errorf("invalid selector entry %q: empty key", pair)
		}
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return nil, fmt.Errorf("invalid label key %q: %s", key, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return nil, fmt.Errorf("invalid label value %q for key %q: %s", value, key, strings.Join(errs, "; "))
		}
		if _, exists := matchLabels[key]; exists {
			return nil, fmt.Errorf("duplicate label key %q", key)
		}

		matchLabels[key] = value
	}

	if len(matchLabels) == 0 {
		return nil, fmt.Errorf("empty selector")
	}
	return matchLabels, nil
}

// matchesMatchLabels reports whether every selector label is present on lbls.
func matchesMatchLabels(selector, lbls map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	return labels.SelectorFromSet(labels.Set(selector)).Matches(labels.Set(lbls))
}
