#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>
#include <stdatomic.h>

/* No payload is printed: only access schedules, timings and physical identities. */
#define N 1024
#define P 4096
static volatile unsigned char *buf;
static atomic_int bg_active;
static pthread_t bg;
static uint64_t ns(void) { struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t); return (uint64_t)t.tv_sec*1000000000+t.tv_nsec; }
static void die(const char *s) { perror(s); exit(1); }
static void touch(char op, unsigned start, unsigned count) {
  volatile unsigned char sink=0;
  for(unsigned i=start; i<start+count; i++) {
    if(op=='w') buf[i*P] = (unsigned char)i; else sink ^= buf[i*P];
  }
  (void)sink;
}
static void *reader(void *unused) {
  (void)unused;
  while(atomic_load(&bg_active)) { touch('r',0,N/4); usleep(1000); }
  return NULL;
}
static void mapping(const char *kind, volatile unsigned char *base, unsigned count) {
  int fd=open("/proc/self/pagemap",O_RDONLY); if(fd<0) die("pagemap");
  uint64_t entries[N];
  for(unsigned i=0;i<count;i++) { if(pread(fd,&entries[i],8,((uintptr_t)base/P+i)*8)!=8)die("pread pagemap"); if(!(entries[i]&(1ULL<<63)) || !(entries[i]&((1ULL<<55)-1))) { fprintf(stderr,"target PFN absent or masked (present=%d uid=%u)\n",!!(entries[i]&(1ULL<<63)),getuid()); exit(2); } }
  printf("{\"event\":\"mapping\",\"kind\":\"%s\",\"guest_monotonic_ns\":%lu,\"page_bytes\":4096,\"pid\":%d,\"gpas\":[",kind,(unsigned long)ns(),getpid());
  for(unsigned i=0;i<count;i++) printf("%s%lu",i?",":"",(unsigned long)((entries[i]&((1ULL<<55)-1))*P));
  puts("]}"); close(fd);
}
int main(void) {
  setvbuf(stdout,NULL,_IOLBF,0);
  int idle_fd=open("/sys/kernel/mm/page_idle/bitmap",O_RDWR); int idle_errno=idle_fd<0?errno:0;
  if(idle_fd>=0)close(idle_fd);
  printf("{\"event\":\"capability\",\"uid\":%u,\"guest_idle_open_errno\":%d}\n",getuid(),idle_errno);
  buf=mmap(NULL,N*P,PROT_READ|PROT_WRITE,MAP_PRIVATE|MAP_ANONYMOUS,-1,0); if(buf==MAP_FAILED)die("mmap");
  if(madvise((void*)buf,N*P,MADV_NOHUGEPAGE))die("madvise");
  touch('w',0,N); usleep(100000); mapping("anonymous",buf,N);
  puts("{\"event\":\"ready\",\"protocol\":\"r|w START COUNT; n; reuse; bg-start; bg-stop; file; quit\"}");
  char line[128];
  while(fgets(line,sizeof(line),stdin)) {
    char op=0; unsigned start=0,count=0; uint64_t begin=ns();
    if(!strncmp(line,"quit",4))break;
    if(!strncmp(line,"bg-start",8)) { if(!atomic_exchange(&bg_active,1) && pthread_create(&bg,NULL,reader,NULL))die("pthread_create"); }
    else if(!strncmp(line,"bg-stop",7)) { if(atomic_exchange(&bg_active,0))pthread_join(bg,NULL); }
    else if(!strncmp(line,"reuse",5)) { if(atomic_load(&bg_active)) { fprintf(stderr,"stop background before reuse\n");return 2;} if(madvise((void*)buf,N*P,MADV_DONTNEED))die("discard");touch('w',0,N);mapping("reuse",buf,N); }
    else if(!strncmp(line,"file",4)) {
      char path[]="/tmp/aries-access-fixture-XXXXXX"; int fd=mkstemp(path); if(fd<0)die("mkstemp"); unlink(path);
      if(ftruncate(fd,N*P))die("ftruncate");
      unsigned char *f=mmap(NULL,N*P,PROT_READ|PROT_WRITE,MAP_SHARED,fd,0);if(f==MAP_FAILED)die("file mmap");
      memset(f,1,N*P); if(msync(f,N*P,MS_SYNC))die("msync"); mapping("filecache",f,N);
      /* Keep file mapping alive until a separate command: file-read uses pread. */
      puts("{\"event\":\"file-ready\"}");
      while(fgets(line,sizeof(line),stdin)) { if(!strncmp(line,"file-end",8))break; unsigned char scratch[P];uint64_t t=ns(); for(unsigned i=0;i<N/4;i++)if(pread(fd,scratch,P,i*P)!=P)die("file read");printf("{\"event\":\"done\",\"op\":\"file-read\",\"begin_ns\":%lu,\"end_ns\":%lu}\n",(unsigned long)t,(unsigned long)ns()); }
      munmap(f,N*P);close(fd);
    }
    else if(sscanf(line,"%c %u %u",&op,&start,&count)==3 && (op=='r'||op=='w') && start<=N && count<=N-start)touch(op,start,count);
    else if(line[0]!='n') { fprintf(stderr,"invalid command\n");return 2; }
    printf("{\"event\":\"done\",\"begin_ns\":%lu,\"end_ns\":%lu}\n",(unsigned long)begin,(unsigned long)ns());
  }
  if(atomic_exchange(&bg_active,0))pthread_join(bg,NULL);
  munmap((void*)buf,N*P); return 0;
}
