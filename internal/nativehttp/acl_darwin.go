//go:build darwin && cgo

package nativehttp

/*
#include <sys/types.h>
#include <sys/acl.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <stdlib.h>
#include <errno.h>

// Use the supported libc API rather than depending on a raw syscall ABI
// or parsing command output. Conservatively refuse every allow ACE,
// including owner-only/inherit-only grants; deny ACEs cannot widen access.
static int native_acl_check(const char *path) {
    filesec_t security = filesec_init();
    if (security == NULL) return errno ? errno : EIO;
    struct stat info;
    if (lstatx_np(path, &info, security) != 0) {
        int saved = errno;
        filesec_free(security);
        return saved ? saved : EIO;
    }
    int present = 0;
    if (filesec_query_property(security, FILESEC_ACL, &present) != 0) {
        int saved = errno;
        filesec_free(security);
        return saved ? saved : EIO;
    }
    if (!present) {
        filesec_free(security);
        return 0;
    }
    acl_t acl = NULL;
    if (filesec_get_property(security, FILESEC_ACL, &acl) != 0 || acl == NULL) {
        int saved = errno;
        filesec_free(security);
        return saved ? saved : EIO;
    }
    filesec_free(security);
    if (acl_valid(acl) != 0) {
        int saved = errno;
        acl_free(acl);
        return saved ? saved : EIO;
    }
    acl_entry_t entry;
    int cursor = ACL_FIRST_ENTRY;
    for (;;) {
        if (acl_get_entry(acl, cursor, &entry) != 0) {
            int saved = errno;
            acl_free(acl);
            // Darwin documents EINVAL after the final entry.
            return saved == EINVAL ? 0 : (saved ? saved : EIO);
        }
        cursor = ACL_NEXT_ENTRY;
        acl_tag_t tag;
        if (acl_get_tag_type(entry, &tag) != 0) {
            int saved = errno;
            acl_free(acl);
            return saved ? saved : EIO;
        }
        if (tag != ACL_EXTENDED_DENY) {
            acl_free(acl);
            return EACCES;
        }
    }
}
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func checkACL(path string) error {
	var mount unix.Statfs_t
	if err := unix.Statfs(path, &mount); err != nil {
		return fmt.Errorf("native HTTP: filesystem permissions: %w", err)
	}
	if mount.Flags&unix.MNT_IGNORE_OWNERSHIP != 0 {
		return fmt.Errorf("native HTTP: filesystem ignores ownership")
	}
	name := C.CString(path)
	defer C.free(unsafe.Pointer(name))
	if code := C.native_acl_check(name); code != 0 {
		return fmt.Errorf("native HTTP: private ACL %s: %w", path, syscall.Errno(code))
	}
	return nil
}
