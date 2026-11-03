#include "testlib.h"
int main(int argc, char* argv[]) {
    registerTestlibCmd(argc, argv);
    int n = inf.readInt();
    int good = 0;
    for (int i = 0; i < n; i++) {
        long long want = ans.readLong();
        long long got = ouf.readLong();
        if (want == got) good++;
    }
    if (good == n) quitf(_ok, "all %d positions correct", n);
    if (good == 0) quitf(_wa, "no position correct");
    quitp(double(good) / n, "%d of %d positions correct", good, n);
}
