# Write 1280x720 yuv420p frames to stdout at 30fps; each frame carries the write
# time (ms) as 44 bits in two rows of 24px luma blocks, matching latency.ts.
import sys, time
W, H, B, BITS = 1280, 720, 24, 44
uv = bytes([128]) * (W * H // 2)
out = sys.stdout.buffer
next_t = time.time()
while True:
    t = int(time.time() * 1000)
    y = bytearray([90]) * (W * H)
    for i in range(BITS):
        bit = (t >> i) & 1
        for row, val in ((0, 255 if bit else 0), (1, 0 if bit else 255)):
            blk = bytes([val]) * B
            for yy in range(row * B, row * B + B):
                o = yy * W + i * B
                y[o:o + B] = blk
    out.write(y); out.write(uv); out.flush()
    next_t += 1 / 30
    time.sleep(max(0, next_t - time.time()))
