#include <vector>
#include <cstdio>
int main(){ std::vector<char> v(1ull<<30, 1); printf("%d\n", v[12345]); }
