#define _GNU_SOURCE
#include <sys/mman.h>
#include <sys/prctl.h>
#include <fcntl.h>
#include <unistd.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
void *worker(void *x) { prctl(PR_SET_NAME,"attr-worker",0,0,0); while(1)sleep(1); return x; }
int main(void) {
 prctl(PR_SET_NAME,"attr-fixture",0,0,0);
 size_t p=4096;
 char *a=mmap(0,8*p,PROT_READ|PROT_WRITE,MAP_PRIVATE|MAP_ANONYMOUS,-1,0);
 int fd=open("/tmp/attrfixture-disk",O_CREAT|O_RDWR|O_TRUNC,0600);
 if(fd<0||ftruncate(fd,4*p))return 2;
 char *s=mmap(0,4*p,PROT_READ|PROT_WRITE,MAP_SHARED,fd,0);
 char *v=mmap(0,4*p,PROT_READ|PROT_WRITE,MAP_PRIVATE,fd,0);
 if(a==MAP_FAILED||s==MAP_FAILED||v==MAP_FAILED)return 3;
 for(int i=0;i<8;i++)sprintf(a+i*p,"ANON-%02d-unique-attribution",i);
 for(int i=0;i<4;i++)sprintf(s+i*p,"FILE-%02d-unique-attribution",i);
 for(int i=0;i<2;i++)sprintf(v+i*p,"COPY-%02d-unique-attribution",i);
 if(link("/tmp/attrfixture-disk","/tmp/attrfixture-link"))return 5;
 pthread_t th; if(pthread_create(&th,0,worker,0))return 4;
 volatile char sink=v[2*p]+v[3*p];(void)sink;
 int dfd=open("/tmp/attrfixture-deleted",O_CREAT|O_RDWR|O_TRUNC,0600);if(dfd<0||ftruncate(dfd,p))return 7;
 char *del=mmap(0,p,PROT_READ|PROT_WRITE,MAP_SHARED,dfd,0);if(del==MAP_FAILED)return 8;
 sprintf(del,"DELETED-00-unique-attribution");if(unlink("/tmp/attrfixture-deleted"))return 9;
 char *stressenv=getenv("ATTR_STRESS_MIB"); size_t stress=stressenv?strtoul(stressenv,0,10)*1024*1024:0;
 if(stress){char *b=mmap(0,stress,PROT_READ|PROT_WRITE,MAP_PRIVATE|MAP_ANONYMOUS,-1,0);if(b==MAP_FAILED)return 6;memset(b,0x5a,stress);}
 printf("{\"pid\":%d,\"anon\":%lu,\"shared\":%lu,\"private\":%lu,\"deleted\":%lu}\n",getpid(),(unsigned long)a,(unsigned long)s,(unsigned long)v,(unsigned long)del);fflush(stdout);
 FILE *in=fopen("/proc/self/maps","r"),*out=fopen("/tmp/fixture-maps","w");char line[1024];while(fgets(line,sizeof(line),in))fputs(line,out);fclose(in);fclose(out);
 while(1)sleep(1);
}
