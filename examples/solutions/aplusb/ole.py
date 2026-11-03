import sys
s = "x" * (1 << 20)
while True:
    sys.stdout.write(s)
