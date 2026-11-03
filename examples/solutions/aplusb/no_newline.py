import sys
a, b = map(int, input().split())
sys.stdout.write(str(a + b))   # no final newline: accepted even in strict mode
