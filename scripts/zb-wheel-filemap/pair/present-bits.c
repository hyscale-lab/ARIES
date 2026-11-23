#define _FILE_OFFSET_BITS 64
#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

/* Emit only present bits, low bit first, without reading the VMM's RAM. */
int main(int argc, char **argv) {
    if (argc != 4 || sysconf(_SC_PAGESIZE) != 4096) return 2;
    char *end;
    unsigned long pid = strtoul(argv[1], &end, 10);
    if (*end || !pid) return 2;
    uint64_t address = strtoull(argv[2], &end, 10);
    if (*end || (address % 4096)) return 2;
    uint64_t pages = strtoull(argv[3], &end, 10);
    if (*end || !pages || (pages % 8) || pages > (UINT64_C(8)<<18)) return 2;
    char path[64];
    snprintf(path, sizeof(path), "/proc/%lu/pagemap", pid);
    int fd = open(path, O_RDONLY|O_CLOEXEC);
    if (fd < 0) {perror(path); return 1;}
    uint64_t entries[8192];
    unsigned char bits[1024];
    for (uint64_t i = 0; i < pages; ) {
        size_t count = pages-i < 8192 ? pages-i : 8192;
        size_t bytes = count * sizeof(entries[0]);
        off_t offset=(off_t)((address/4096+i)*sizeof(entries[0]));
        ssize_t nr;
        do { nr=pread(fd,entries,bytes,offset); } while (nr<0 && errno==EINTR);
        if (nr<0 || (size_t)nr!=bytes) { perror("incomplete pagemap scan"); return 1; }
        for (size_t j=0; j<count; j+=8) {
            unsigned char byte=0;
            for (unsigned int k=0;k<8;k++) byte|=(unsigned char)((entries[j+k]>>63)<<k);
            bits[j/8]=byte;
        }
        if (fwrite(bits,1,count/8,stdout)!=count/8) return 1;
        i+=count;
    }
    return fclose(stdout)!=0 || close(fd)!=0;
}
