// Trampolines to functions of libSystem (see libc_darwin.go),
// as golang.org/x/sys/unix declares its own.

#include "textflag.h"

TEXT libc_mkfifoat_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mkfifoat(SB)
GLOBL	·libc_mkfifoat_trampoline_addr(SB), RODATA, $8
DATA	·libc_mkfifoat_trampoline_addr(SB)/8, $libc_mkfifoat_trampoline<>(SB)

TEXT libc_acl_get_fd_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_acl_get_fd_np(SB)
GLOBL	·libc_acl_get_fd_np_trampoline_addr(SB), RODATA, $8
DATA	·libc_acl_get_fd_np_trampoline_addr(SB)/8, $libc_acl_get_fd_np_trampoline<>(SB)

TEXT libc_acl_to_text_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_acl_to_text(SB)
GLOBL	·libc_acl_to_text_trampoline_addr(SB), RODATA, $8
DATA	·libc_acl_to_text_trampoline_addr(SB)/8, $libc_acl_to_text_trampoline<>(SB)

TEXT libc_acl_free_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_acl_free(SB)
GLOBL	·libc_acl_free_trampoline_addr(SB), RODATA, $8
DATA	·libc_acl_free_trampoline_addr(SB)/8, $libc_acl_free_trampoline<>(SB)
