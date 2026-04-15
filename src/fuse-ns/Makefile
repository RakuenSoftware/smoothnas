CC      = cc
# Use pkg-config when available; fall back to hardcoded flags for systems
# where pkg-config is not installed (e.g. minimal Debian/Proxmox targets).
FUSE3_CFLAGS ?= $(shell pkg-config --cflags fuse3 2>/dev/null || echo -I/usr/include/fuse3 -D_FILE_OFFSET_BITS=64)
FUSE3_LIBS   ?= $(shell pkg-config --libs   fuse3 2>/dev/null || echo -lfuse3 -lpthread)
CFLAGS  = -Wall -Wextra -O2 $(FUSE3_CFLAGS)
LDFLAGS = $(FUSE3_LIBS)
TARGET  = tierd-fuse-ns

.PHONY: all clean install

all: $(TARGET)

$(TARGET): fuse_ns.c
	$(CC) $(CFLAGS) -o $@ $< $(LDFLAGS)

install: $(TARGET)
	install -m 755 $(TARGET) /usr/local/bin/$(TARGET)

clean:
	rm -f $(TARGET)
