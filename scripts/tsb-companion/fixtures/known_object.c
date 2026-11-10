// SPDX-License-Identifier: Apache-2.0
#define _GNU_SOURCE
#include <sys/mman.h>
#include <sys/prctl.h>
#include <stdio.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
static uint64_t pfn(void *p) {
 uint64_t x; int fd=open("/proc/self/pagemap",O_RDONLY);
 if(fd<0 || pread(fd,&x,8,((uintptr_t)p/4096)*8)!=8)exit(20);
 close(fd);return x&((1ULL<<55)-1);
}
int main(void) {
 setvbuf(stdout,NULL,_IONBF,0);prctl(PR_SET_NAME,"aries-attrib",0,0,0);
 void *p;if(posix_memalign(&p,4096,16384))return 2;
 memset(p,0,16384);mlock(p,16384);
 int f=open("/known-file",O_RDONLY);if(f<0)return 3;
 unsigned char *file=mmap(NULL,4096,PROT_READ,MAP_PRIVATE,f,0);close(f);
 if(file==MAP_FAILED)return 4;
 volatile unsigned char first=file[0];
 printf("READY pid=%d file_pfn=%llu anon_pfns=%llu,%llu,%llu,%llu first=%u\n",
   getpid(),(unsigned long long)pfn(file),(unsigned long long)pfn(p),
   (unsigned long long)pfn(p+4096),(unsigned long long)pfn(p+8192),(unsigned long long)pfn(p+12288),first);
 char cmd[64];
 while(fgets(cmd,sizeof(cmd),stdin)) {
  if(strncmp(cmd,"touch",5))continue;
  volatile unsigned char *v=p;unsigned char a=v[0];v[4096]=77;v[8192]=99;v[8192]=0;
  unsigned char b=((volatile unsigned char*)file)[0];
  int block=open("/dev/vda",O_RDONLY|O_DIRECT);ssize_t n=pread(block,p+12288,4096,0);close(block);
  printf("DONE pid=%d file_pfn=%llu file=%u read=%u restored=%u block=%zd\n",
    getpid(),(unsigned long long)pfn(file),b,a,v[8192],n);
 }
 return 0;
}
