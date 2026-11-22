// A stock-KVM calibration VM touching one byte in a private host file mapping.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <linux/kvm.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <unistd.h>

static void require(int ok, const char *operation)
{
    if (!ok) {
        perror(operation);
        exit(1);
    }
}

int main(int argc, char **argv)
{
    if (argc != 2 || (strcmp(argv[1], "warm") != 0 && strcmp(argv[1], "cold") != 0)) {
        fprintf(stderr, "use test_vm warm|cold\n");
        return 1;
    }
    int cold = strcmp(argv[1], "cold") == 0;
    int kvm = open("/dev/kvm", O_RDWR | O_CLOEXEC);
    require(kvm >= 0, "open /dev/kvm");
    int vm = ioctl(kvm, KVM_CREATE_VM, 0);
    require(vm >= 0, "KVM_CREATE_VM");
    int cpu = ioctl(vm, KVM_CREATE_VCPU, 0);
    require(cpu >= 0, "KVM_CREATE_VCPU");
    int run_size = ioctl(kvm, KVM_GET_VCPU_MMAP_SIZE, 0);
    require(run_size > 0, "KVM_GET_VCPU_MMAP_SIZE");
    struct kvm_run *run = mmap(NULL, run_size, PROT_READ | PROT_WRITE, MAP_SHARED, cpu, 0);
    require(run != MAP_FAILED, "map CPU run data");

    char private_path[] = ".zb-demand-calibration-XXXXXX";
    int backing = mkstemp(private_path);
    require(backing >= 0, "create private backing");
    require(unlink(private_path) == 0, "unlink private backing");
    const size_t page = 4096;
    unsigned char program[4096] = {0xc6, 0x06, 0x00, 0x08, 0x55, 0xf4};
    require(write(backing, program, page) == (ssize_t)page, "write backing");
    require(fsync(backing) == 0, "fsync backing");
    if (cold) {
        int error = posix_fadvise(backing, 0, page, POSIX_FADV_DONTNEED);
        errno = error;
        require(error == 0, "advise private backing");
    }
    unsigned char *hva = mmap(NULL, page, PROT_READ | PROT_WRITE, MAP_PRIVATE, backing, 0);
    require(hva != MAP_FAILED, "map untouched private file");
    unsigned char residency = 0;
    require(mincore(hva, page, &residency) == 0, "measure cache residency");
    uint64_t pagemap = 0;
    int pm = open("/proc/self/pagemap", O_RDONLY | O_CLOEXEC);
    require(pm >= 0, "open calibration pagemap");
    off_t entry = (uintptr_t)hva / page * sizeof(pagemap);
    require(pread(pm, &pagemap, sizeof(pagemap), entry) == sizeof(pagemap),
            "read initial pagemap entry");
    close(pm);
    if (pagemap >> 63) {
        fprintf(stderr, "calibration requires an untouched host mapping\n");
        return 1;
    }
    struct kvm_userspace_memory_region region = {
        .slot = 0,
        .guest_phys_addr = 0,
        .memory_size = page,
        .userspace_addr = (uintptr_t)hva,
    };
    require(ioctl(vm, KVM_SET_USER_MEMORY_REGION, &region) == 0, "register guest RAM");
    struct kvm_sregs sregs;
    require(ioctl(cpu, KVM_GET_SREGS, &sregs) == 0, "get CPU segments");
    sregs.cs.base = 0;
    sregs.cs.selector = 0;
    require(ioctl(cpu, KVM_SET_SREGS, &sregs) == 0, "set CPU segments");
    struct kvm_regs regs = {.rip = 0, .rflags = 2};
    require(ioctl(cpu, KVM_SET_REGS, &regs) == 0, "set CPU registers");
    require(prctl(PR_SET_NAME, "fc_vcpu 0", 0, 0, 0) == 0, "set calibration thread name");
    require(ioctl(cpu, KVM_RUN, 0) == 0, "run calibration CPU");
    if (run->exit_reason != KVM_EXIT_HLT || hva[0x800] != 0x55) {
        fprintf(stderr, "unexpected calibration exit or guest write\n");
        return 1;
    }
    printf("{\"mode\":\"%s\",\"pid\":%d,\"hva\":%" PRIuPTR
           ",\"guest_phys_addr\":0,\"host_pte_present_before\":false,"
           "\"cache_resident_before\":%u,\"guest_write_observed\":true}\n",
           argv[1], getpid(), (uintptr_t)hva, residency & 1);
    return 0;
}
