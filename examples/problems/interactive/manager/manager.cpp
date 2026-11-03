#include <cstdio>
#include <cstring>
#include <unistd.h>
static void finish(double f, const char* msg) {
    FILE* s = fopen("score.txt", "w"); fprintf(s, "%.6f\n", f); fclose(s);
    fprintf(stderr, "%s\n", msg); fflush(stdout); _exit(0);
}
int main(int argc, char** argv) {
    FILE* in = fopen(argv[1], "r"); long long n; fscanf(in, "%lld", &n); fclose(in);
    for (int q = 0; q <= 40; q++) {
        char op[4]; long long x;
        if (scanf("%3s %lld", op, &x) != 2) finish(0, "wrong answer: unexpected end of interaction");
        if (!strcmp(op, "!")) finish(x == n ? 1 : 0, x == n ? "ok guessed" : "wrong answer: wrong guess");
        if (q == 40) break;
        printf("%s\n", x < n ? "<" : x > n ? ">" : "="); fflush(stdout);
    }
    finish(0, "wrong answer: too many queries");
}
