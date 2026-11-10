#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/userfaultfd.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <sys/un.h>
#include <unistd.h>

#define CHECK(x) do { if (!(x)) { perror(#x); return 1; } } while (0)
int main(int argc, char **argv) {
    CHECK(argc == 2);
    setbuf(stdout, NULL);
    unsigned char *mem = mmap(NULL, 16384, PROT_READ|PROT_WRITE, MAP_PRIVATE|MAP_ANONYMOUS, -1, 0);
    CHECK(mem != MAP_FAILED);
    int fd = syscall(SYS_userfaultfd, O_CLOEXEC|O_NONBLOCK);
    CHECK(fd >= 0);
    struct uffdio_api api = {.api=UFFD_API};
    CHECK(ioctl(fd, UFFDIO_API, &api) == 0);
    struct uffdio_register reg = {.range={.start=(uintptr_t)mem,.len=16384},.mode=UFFDIO_REGISTER_MODE_MISSING};
    CHECK(ioctl(fd, UFFDIO_REGISTER, &reg) == 0);
    int sock = socket(AF_UNIX,SOCK_STREAM,0);
    CHECK(sock >= 0);
    struct sockaddr_un addr = {.sun_family=AF_UNIX};
    CHECK(strlen(argv[1]) < sizeof(addr.sun_path));
    strcpy(addr.sun_path,argv[1]);
    CHECK(connect(sock,(struct sockaddr*)&addr,sizeof(addr)) == 0);
    char json[256];
    int len=snprintf(json,sizeof(json),"[{\"base_host_virt_addr\":%llu,\"size\":16384,\"offset\":0,\"page_size\":4096}]",(unsigned long long)(uintptr_t)mem);
    struct iovec io={.iov_base=json,.iov_len=(size_t)len};
    char control[CMSG_SPACE(sizeof(int))]={0};
    struct msghdr msg={.msg_iov=&io,.msg_iovlen=1,.msg_control=control,.msg_controllen=sizeof(control)};
    struct cmsghdr *c=CMSG_FIRSTHDR(&msg);
    c->cmsg_level=SOL_SOCKET;c->cmsg_type=SCM_RIGHTS;c->cmsg_len=CMSG_LEN(sizeof(int));
    memcpy(CMSG_DATA(c),&fd,sizeof(fd));
    CHECK(sendmsg(sock,&msg,0)==len);
    puts("ready");
    char line[64]; volatile unsigned char *p=mem;
    while(fgets(line,sizeof(line),stdin)) {
        if(line[0]=='1') {
            unsigned char a=p[0]; p[4096]=99; unsigned char b=p[0];
            printf("first %u %u %u\n",a,b,p[4096]);
        } else if(line[0]=='2') {
            unsigned char a=p[8192]; unsigned char b=p[0];
            printf("second %u %u\n",a,b);
        } else if(line[0]=='q') break;
    }
    CHECK(munmap(mem,16384)==0);close(sock);close(fd);return 0;
}
