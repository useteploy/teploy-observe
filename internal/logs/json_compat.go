package logs

import "encoding/json"

func jsonMarshalCompat(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
