#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <unistd.h>
#include <string.h>
#include <fcntl.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <linux/vm_sockets.h>
static uint64_t pfn(void *p){uint64_t x=0;int f=open("/proc/self/pagemap",O_RDONLY);if(pread(f,&x,8,((uintptr_t)p/4096)*8)!=8)exit(20);close(f);return x&((1ULL<<55)-1);}
int main(){setvbuf(stdout,NULL,_IONBF,0);
if(!fork()){int s=socket(AF_VSOCK,SOCK_STREAM,0);struct sockaddr_vm a={.svm_family=AF_VSOCK,.svm_cid=VMADDR_CID_ANY,.svm_port=5000};if(bind(s,(void*)&a,sizeof(a))||listen(s,4)){perror("vsock");_exit(8);}puts("VSLISTEN");int x=accept(s,0,0);puts("VSREADY");char b[4096];while(read(x,b,sizeof(b))>0){} _exit(0);}
void *p; if(posix_memalign(&p,4096,20480))return 2;memset(p,0,20480);mlock(p,20480);if(madvise(p,20480,MADV_DONTFORK))return 3;
printf("CONTROL_PFN %llu\n",(unsigned long long)pfn(p+16384));printf("PFNS %llu %llu %llu %llu\n",(unsigned long long)pfn(p),(unsigned long long)pfn(p+4096),(unsigned long long)pfn(p+8192),(unsigned long long)pfn(p+12288));
unsigned seq=0;char b[64];while(fgets(b,sizeof(b),stdin)){if(!strncmp(b,"markers",7)){volatile unsigned char *v=p;unsigned char x=v[0];v[4096]=77;v[8192]=99;v[8192]=0;int f=open("/dev/vda",O_RDONLY|O_DIRECT);ssize_t n=pread(f,p+12288,4096,0);close(f);printf("POST_PFNS %llu %llu %llu %llu\n",(unsigned long long)pfn(p),(unsigned long long)pfn(p+4096),(unsigned long long)pfn(p+8192),(unsigned long long)pfn(p+12288));printf("MARKERS_DONE seq=%u read=%u restore=%u block=%zd first=%u\n",++seq,x,v[8192],n,v[12288]);}else puts("READY");}return 0;}
