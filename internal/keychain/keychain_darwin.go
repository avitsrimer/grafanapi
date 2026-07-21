//go:build darwin

package keychain

/*
#cgo CFLAGS: -x objective-c -fno-objc-arc
#cgo LDFLAGS: -framework Security -framework Foundation -framework CoreFoundation
#include <stdlib.h>
#include <Security/Security.h>

// makeData wraps a Go byte slice as a CFDataRef the caller must release.
static CFDataRef makeData(const void *bytes, int len) {
	return CFDataCreate(kCFAllocatorDefault, (const UInt8 *)bytes, len);
}

// makeString wraps a NUL-terminated C string as a CFStringRef the caller must release.
static CFStringRef makeString(const char *s) {
	return CFStringCreateWithCString(kCFAllocatorDefault, s, kCFStringEncodingUTF8);
}

// setItem stores secret under service/account as a plain generic-password item, atomically:
// it first attempts SecItemUpdate against the existing item, and only falls back to SecItemAdd
// (creating a fresh item with no access-control and no accessibility attribute, so it lands in
// the default (login) keychain with the creating binary's ad-hoc code identity added to its ACL)
// when no item exists yet (errSecItemNotFound). There is deliberately no SecItemDelete step: a
// delete-then-add sequence has a window where, if SecItemAdd then failed, the previously-stored
// secret would already be gone with no way to recover it. Update-or-add has no such window — a
// failing SecItemUpdate leaves the existing item untouched, and a failing SecItemAdd never had
// a prior item to destroy. Returns the OSStatus (errSecSuccess on success).
static int setItem(const char *service, const char *account, const void *secret, int secretLen) {
	CFStringRef svc = makeString(service);
	CFStringRef acct = makeString(account);
	CFDataRef val = makeData(secret, secretLen);

	CFMutableDictionaryRef query = CFDictionaryCreateMutable(kCFAllocatorDefault, 0,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFDictionarySetValue(query, kSecClass, kSecClassGenericPassword);
	CFDictionarySetValue(query, kSecAttrService, svc);
	CFDictionarySetValue(query, kSecAttrAccount, acct);

	CFMutableDictionaryRef update = CFDictionaryCreateMutable(kCFAllocatorDefault, 0,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFDictionarySetValue(update, kSecValueData, val);

	OSStatus status = SecItemUpdate(query, update);
	if (status == errSecItemNotFound) {
		// no existing item to update: add a fresh one, reusing the query dictionary as its
		// class/service/account attributes.
		CFDictionarySetValue(query, kSecValueData, val);
		status = SecItemAdd(query, NULL);
	}

	CFRelease(update);
	CFRelease(query);
	CFRelease(val);
	CFRelease(acct);
	CFRelease(svc);
	return (int)status;
}

// getItem reads the secret for service/account with a plain SecItemCopyMatching. The creating
// binary's ad-hoc code identity is trusted by the item's ACL and reads it back silently; a binary
// with a different code identity (e.g. after a rebuild, since the ad-hoc cdhash changes) triggers
// the standard keychain "Allow / Always Allow" prompt. On success it returns errSecSuccess and
// sets *outData / *outLen to a malloc'd buffer the caller must free.
static int getItem(const char *service, const char *account, void **outData, int *outLen) {
	CFStringRef svc = makeString(service);
	CFStringRef acct = makeString(account);

	CFMutableDictionaryRef q = CFDictionaryCreateMutable(kCFAllocatorDefault, 0,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFDictionarySetValue(q, kSecClass, kSecClassGenericPassword);
	CFDictionarySetValue(q, kSecAttrService, svc);
	CFDictionarySetValue(q, kSecAttrAccount, acct);
	CFDictionarySetValue(q, kSecReturnData, kCFBooleanTrue);
	CFDictionarySetValue(q, kSecMatchLimit, kSecMatchLimitOne);

	CFDataRef result = NULL;
	OSStatus status = SecItemCopyMatching(q, (CFTypeRef *)&result);
	CFRelease(q);
	CFRelease(acct);
	CFRelease(svc);

	if (status == errSecSuccess && result != NULL) {
		CFIndex n = CFDataGetLength(result);
		void *buf = malloc((size_t)n);
		if (buf != NULL) {
			CFDataGetBytes(result, CFRangeMake(0, n), (UInt8 *)buf);
			*outData = buf;
			*outLen = (int)n;
		} else {
			status = errSecAllocate;
		}
	}
	if (result != NULL) {
		CFRelease(result);
	}
	return (int)status;
}

// deleteItem removes the item for service/account. errSecItemNotFound is treated as success by
// the Go caller. Returns the OSStatus.
static int deleteItem(const char *service, const char *account) {
	CFStringRef svc = makeString(service);
	CFStringRef acct = makeString(account);

	CFMutableDictionaryRef del = CFDictionaryCreateMutable(kCFAllocatorDefault, 0,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFDictionarySetValue(del, kSecClass, kSecClassGenericPassword);
	CFDictionarySetValue(del, kSecAttrService, svc);
	CFDictionarySetValue(del, kSecAttrAccount, acct);

	OSStatus status = SecItemDelete(del);
	CFRelease(del);
	CFRelease(acct);
	CFRelease(svc);
	return (int)status;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// errSecItemNotFound mirrors the Security framework status for a missing item.
const errSecItemNotFound = -25300

// newStore returns the cgo-backed darwin Store.
//
// We have to return an interface here.
//
//nolint:ireturn
func newStore() Store {
	return darwinStore{}
}

// darwinStore is the cgo-backed Store. Items are plain generic-password entries in the default
// (login) keychain; the item's trusted-application ACL is bound to the ad-hoc code identity of
// the binary that created it, so that same binary reads it back without a prompt. It is not safe
// for concurrent use on the same account.
type darwinStore struct{}

// Set stores secret under account as a plain generic-password item in the default (login)
// keychain. It does not prompt. It is atomic (update-or-add at the cgo layer, no
// delete-then-add): a failing Set leaves any previously stored secret for account intact.
func (darwinStore) Set(account, secret string) error {
	cSvc := C.CString(Service)
	defer C.free(unsafe.Pointer(cSvc))
	cAcct := C.CString(account)
	defer C.free(unsafe.Pointer(cAcct))

	val := []byte(secret)
	var valPtr unsafe.Pointer
	if len(val) > 0 {
		valPtr = unsafe.Pointer(&val[0])
	}
	status := C.setItem(cSvc, cAcct, valPtr, C.int(len(val)))
	if int(status) != 0 {
		return fmt.Errorf("keychain set for account %q failed: OSStatus %d", account, int(status))
	}
	return nil
}

// Get reads the secret for account. The ad-hoc code identity of the binary that created the item
// is trusted by its ACL and reads it back silently; a binary with a different code identity hits
// the standard keychain "Allow / Always Allow" prompt.
func (darwinStore) Get(account string) (string, error) {
	cSvc := C.CString(Service)
	defer C.free(unsafe.Pointer(cSvc))
	cAcct := C.CString(account)
	defer C.free(unsafe.Pointer(cAcct))

	var data unsafe.Pointer
	var n C.int
	status := C.getItem(cSvc, cAcct, &data, &n) //nolint:gocritic // dupSubExpr false positive on cgo-generated call
	if int(status) != 0 {
		if int(status) == errSecItemNotFound {
			return "", fmt.Errorf("keychain get for account %q: %w", account, ErrNotFound)
		}
		return "", fmt.Errorf("keychain get for account %q failed: OSStatus %d", account, int(status))
	}
	if data == nil {
		return "", fmt.Errorf("keychain get for account %q: %w", account, ErrNotFound)
	}
	defer C.free(data)
	secret := C.GoStringN((*C.char)(data), n)
	return secret, nil
}

// Delete removes the item for account. A missing item is reported as success.
func (darwinStore) Delete(account string) error {
	cSvc := C.CString(Service)
	defer C.free(unsafe.Pointer(cSvc))
	cAcct := C.CString(account)
	defer C.free(unsafe.Pointer(cAcct))

	status := C.deleteItem(cSvc, cAcct)
	if int(status) != 0 && int(status) != errSecItemNotFound {
		return fmt.Errorf("keychain delete for account %q failed: OSStatus %d", account, int(status))
	}
	return nil
}
