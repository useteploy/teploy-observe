package metrics

import (
	"fmt"
	"google.golang.org/protobuf/encoding/protowire"
)

// ValidateProtoWire bounds packed histogram fragmentation before the pinned
// protobuf fast decoder allocates/copies cumulative slices (scan GRPC-02).
// Ordinary protobuf encodings use one packed fragment. Sixteen fragments per
// field remain supported; highly fragmented exporters must consolidate them.
// This is an Observe admission workaround, not a change to the vendor pin.
func ValidateProtoWire(body []byte) error { return validateMetricWire(body, 0) }

func validateMetricWire(body []byte, level int) error {
	counts := map[protowire.Number]int{}
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return fmt.Errorf("invalid metrics protobuf tag")
		}
		body = body[n:]
		if typ == protowire.BytesType {
			counts[num]++
		}
		if (level == 6 && (num == 6 || num == 7)) || (level == 8 && num == 2) || (level == 7 && (num == 8 || num == 9)) {
			if counts[num] > 16 {
				return fmt.Errorf("histogram field exceeds 16 wire fragments; consolidate packed fields")
			}
		}
		next := -1
		switch level {
		case 0:
			if num == 1 {
				next = 1
			}
		case 1:
			if num == 2 {
				next = 2
			}
		case 2:
			if num == 2 {
				next = 3
			}
		case 3:
			if num == 9 {
				next = 4
			}
			if num == 10 {
				next = 5
			}
		case 4:
			if num == 1 {
				next = 6
			}
		case 5:
			if num == 1 {
				next = 7
			}
		case 7:
			if num == 8 || num == 9 {
				next = 8
			}
		}
		if typ == protowire.BytesType {
			val, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return fmt.Errorf("invalid metrics protobuf bytes")
			}
			if next >= 0 {
				if err := validateMetricWire(val, next); err != nil {
					return err
				}
			}
			body = body[n:]
		} else {
			if next >= 0 {
				return fmt.Errorf("invalid metrics protobuf message type")
			}
			n := protowire.ConsumeFieldValue(num, typ, body)
			if n < 0 {
				return fmt.Errorf("invalid metrics protobuf value")
			}
			body = body[n:]
		}
	}
	return nil
}
