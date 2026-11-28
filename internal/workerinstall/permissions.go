package workerinstall

import (
	"encoding/binary"
	"errors"
)

// The indirection permits a test subprocess to remove this entire guard and
// prove the native ACL regression fails. No caller outside this package can
// replace it, and no request or configuration controls it.
var checkPlatformACL = platformACL

// Darwin returns an attrreference followed by kauth_filesec/kauth_ace records.
// Layout: apple-oss-distributions/xnu bsd/sys/{attr,kauth}.h. Read-only/deny
// entries are safe here; any grant of mutation or unknown semantics refuses,
// including inheritance-only grants. We do not resolve ACL principals.
func safeDarwinACL(b []byte) error {
	bad := errors.New("worker install: extended permissions are unsafe or cannot be established")
	if len(b) < 12 {
		return bad
	}
	size := int(binary.LittleEndian.Uint32(b[:4]))
	if size < 12 || size > len(b) {
		return bad
	}
	offset := int(int32(binary.LittleEndian.Uint32(b[4:8])))
	length := int(binary.LittleEndian.Uint32(b[8:12]))
	if length == 0 {
		return nil
	}
	start := 4 + offset
	if offset < 8 || start > size || length > size-start || length < 44 {
		return bad
	}
	acl := b[start : start+length]
	if binary.LittleEndian.Uint32(acl[:4]) != 0x012cc16d {
		return bad
	}
	count := binary.LittleEndian.Uint32(acl[36:40])
	flags := binary.LittleEndian.Uint32(acl[40:44])
	if flags & ^uint32(0xffff|1<<17) != 0 {
		return bad
	} // Deferred/unknown inheritance is not proof.
	if count == 0xffffffff {
		if length == 44 {
			return nil
		}
		return bad
	}
	if count > 128 || length != 44+24*int(count) {
		return bad
	}
	const readOnlyRights = uint32(1<<1 | 1<<3 | 1<<7 | 1<<9 | 1<<11 | 1<<20 | 1<<22 | 1<<24)
	for i := uint32(0); i < count; i++ {
		entry := acl[44+24*int(i) : 44+24*int(i+1)]
		kind := binary.LittleEndian.Uint32(entry[16:20])
		rights := binary.LittleEndian.Uint32(entry[20:24])
		if kind & ^uint32(0x1ff) != 0 || kind&0xf != 1 && kind&0xf != 2 {
			return bad
		}
		if kind&0xf == 1 && rights & ^readOnlyRights != 0 {
			return bad
		}
	}
	return nil
}
