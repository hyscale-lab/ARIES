# Pinned Linux 5.10.239 layout profile

These three small JSON files describe the original experiment's deployed guest
kernel ELF, SHA-256:

    0e524afe5a90f4ce4b13091a8b5acf7fbe9e7584e711f609d27b69b78564117d

The kernel binary is not included. Supply the ELF actually used to boot the VM;
a matching release string alone is insufficient. The JSON checksums in
SHA256SUMS retain the prior validated profile verbatim.

Base and extended offsets were generated from compiler offsetof/sizeof probes
against amazonlinux/linux source commit
1696dae7327b645d2b4b860e919e431dcda0d11c
(microvm-kernel-5.10.239-261.959.amzn2) using the deployed kernel's embedded
IKCONFIG. The recorded configuration SHA-256 is
4a7f02416d798993f11bfe5896de2c88b7bf82a4bf6cf23e6371fe4ef8cc0522.

The offset-generation compiler was GCC 11.5; the deployed kernel reported GCC
11.4. Feature configuration remained unchanged after olddefconfig, but this does
NOT establish an exact reproducible rebuild of the deployed ELF. Its complete
original build provenance/toolchain is not available in this package. We do not
claim that compiling the named source produces the supplied ELF hash.

Bootstrap compares the ELF hash with the layout, validates KASLR/root-page-table
translations and VMCOREINFO offsets/sizes (18 overlapping checks in the prior
profile), then checks task/VMA traversal and PID1. Owner/name offsets additionally
had native semantic fixtures. Those checks support this pinned profile; they do
not independently prove every generated offset or make it portable to another
kernel.

For a different kernel, generate fresh layout modules from that kernel's actual
build configuration/type information and validate overlapping VMCOREINFO plus
known process/file/anonymous/compound-page fixtures. Do not edit the hash to
force acceptance. Four-level x86-64 paging is supported; LA57 is not.
