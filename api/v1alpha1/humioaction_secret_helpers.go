package v1alpha1

import "fmt"

var HaSecrets map[string]string = make(map[string]string)

func HaHasSecret(hn *HumioAction) (string, bool) {
	if secret, found := HaSecrets[fmt.Sprintf("%s-%s", hn.Namespace, hn.Name)]; found {
		return secret, true
	}
	return "", false
}

// Fanicia TODO: Call this to set the secret. It also removes the secret from the action... which is kind of ugly
// Note that this side effect means we cant use it in resolveFields.
// Consider if this is the way forward or not
func SecretFromHa(hn *HumioAction) {
	key := fmt.Sprintf("%s-%s", hn.Namespace, hn.Name)
	value := hn.Spec.SlackPostMessageProperties.ApiToken
	HaSecrets[key] = value
	hn.Spec.SlackPostMessageProperties.ApiToken = ""
}
