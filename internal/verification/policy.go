package verification

import (
	"strconv"
	"strings"
)

func ValidPolicyOID(raw string) bool {
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return false
	}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		n, err := strconv.ParseUint(part, 10, 31)
		if err != nil || (i == 0 && n > 2) || (i == 1 && parts[0] != "2" && n > 39) {
			return false
		}
	}
	return true
}
