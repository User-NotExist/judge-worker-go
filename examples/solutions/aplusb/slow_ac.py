# Correct but burns ~1.5 s of CPU per testcase: used by the crash/reclaim test.
x = 0
for i in range(25_000_000):
    x += i
a, b = map(int, input().split())
print(a + b)
