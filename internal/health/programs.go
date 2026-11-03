package health

const aPlusBCpp = `#include <cstdio>
int main(){long long a,b;if(scanf("%lld %lld",&a,&b)!=2)return 1;printf("%lld\n",a+b);}
`

const aPlusBPy = `a, b = map(int, input().split())
print(a + b)
`

// benchCpp is a deterministic CPU-bound loop (~0.3–0.6 s) for the timing baseline.
const benchCpp = `#include <cstdio>
#include <cstdint>
int main(){
  uint64_t x=88172645463325252ull, s=0;
  for(long i=0;i<300000000L;i++){ x^=x<<13; x^=x>>7; x^=x<<17; s+=x&1023; }
  printf("%llu\n",(unsigned long long)s);
}
`

const infiniteLoopCpp = `int main(){ volatile unsigned long x=0; for(;;) x++; }
`

const sleepCpp = `#include <unistd.h>
int main(){ sleep(1000); }
`

const alloc2GiBCpp = `#include <cstdlib>
#include <cstring>
#include <cstdio>
int main(){
  size_t n = 2ull<<30; char* p = (char*)malloc(n);
  if(!p){ puts("nomem"); return 0; }
  for(size_t i=0;i<n;i+=4096) p[i]=1;
  printf("%d\n", p[n-4096]);
}
`

const networkCpp = `#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <unistd.h>
#include <cstdio>
#include <cstdlib>
int main(int argc, char** argv){
  int s = socket(AF_INET, SOCK_STREAM, 0);
  if(s<0){ puts("blocked"); return 0; }
  sockaddr_in a{}; a.sin_family=AF_INET; a.sin_port=htons(80);
  inet_pton(AF_INET, "%IP%", &a.sin_addr);
  if(connect(s,(sockaddr*)&a,sizeof a)<0){ puts("blocked"); return 0; }
  puts("connected"); return 0;
}
`

const hiddenCacheCpp = `#include <sys/stat.h>
#include <cstdio>
int main(){
  struct stat st;
  const char* ps[] = {"/var/lib/judge-worker", "/var/lib/judge-worker/cache", "/run/secrets"};
  for(auto p: ps) if(stat(p,&st)==0){ printf("visible %s\n", p); return 0; }
  puts("hidden");
}
`

const forkBombCpp = `#include <unistd.h>
#include <cstdio>
int main(){
  int ok=0;
  for(int i=0;i<10000;i++){ pid_t p=fork(); if(p<0) break; if(p==0){ for(;;) fork(); } ok++; }
  if(ok==0) puts("limited"); else printf("forked %d\n", ok);
}
`

const stdout1GiBCpp = `#include <cstdio>
#include <cstring>
int main(){ static char b[1<<20]; memset(b,'x',sizeof b); for(int i=0;i<1024;i++) fwrite(b,1,sizeof b,stdout); }
`

const deepRecursionCpp = `#include <cstdio>
int f(int n){ volatile char pad[64]; pad[0]=n; return n==0?0:1+f(n-1)+pad[0]*0; }
int main(){ printf("%d\n", f(1000000)); }
`

const readShadowCpp = `#include <cstdio>
int main(){ FILE* f=fopen("/etc/shadow","r"); if(!f){ puts("denied"); return 0; } puts("readable"); }
`

const findAnsCpp = `#include <ftw.h>
#include <cstring>
#include <cstdio>
static int n=0;
static int cb(const char* p, const struct stat*, int, struct FTW*){
  size_t l=strlen(p); if(l>4 && strcmp(p+l-4,".ans")==0) n++; return 0; }
int main(){ nftw("/", cb, 32, FTW_PHYS); printf("%d\n", n); }
`

const fillTmpfsCpp = `#include <cstdio>
#include <cstring>
int main(){
  FILE* f=fopen("/w/fill","wb"); if(!f){ puts("enospc"); return 0; }
  static char b[1<<20]; memset(b,'x',sizeof b);
  for(int i=0;i<4096;i++){ if(fwrite(b,1,sizeof b,f)!=sizeof b){ puts("enospc"); return 0; } }
  if(fflush(f)!=0){ puts("enospc"); return 0; }
  puts("wrote 4 GiB");
}
`

const threadsCpp = `#include <pthread.h>
#include <unistd.h>
#include <cstdio>
void* run(void*){ sleep(5); return nullptr; }
int main(){
  int ok=0; for(int i=0;i<64;i++){ pthread_t t; if(pthread_create(&t,nullptr,run,nullptr)!=0) break; ok++; }
  if(ok==0) puts("limited"); else printf("threads %d\n", ok);
}
`

const exit3Cpp = `int main(){ return 3; }
`

const includeRandomCpp = `#include "/dev/random"
int main(){}
`

const templateBombCpp = `template<int N> struct F { static const long v = F<N-1>::v + F<N-2>::v; };
template<> struct F<1> { static const long v = 1; };
template<> struct F<0> { static const long v = 0; };
int main(){ return F<100000>::v; }
`

// errorFloodCpp produces a very large amount of compiler diagnostics.
const errorFloodCpp = `#define A x x x x x x x x x x x x x x x x
#define B A A A A A A A A A A A A A A A A
#define C B B B B B B B B B B B B B B B B
#define D C C C C C C C C C C C C C C C C
int main(){ D D D D D D D D D D D D D D D D }
`
