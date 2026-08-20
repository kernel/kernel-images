#define _GNU_SOURCE

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#include <wayland-client.h>

#include "weston-screenshooter-client-protocol.h"

struct capture {
	struct wl_display *display;
	struct wl_shm *shm;
	struct wl_output *output;
	struct weston_screenshooter *screenshooter;
	struct wl_buffer *buffer;
	void *pixels;
	size_t size;
	int width;
	int height;
	int done;
};

static volatile sig_atomic_t stopping;

static void
handle_signal(int signal_number)
{
	(void)signal_number;
	stopping = 1;
}

static int
create_anonymous_file(size_t size)
{
	int fd = syscall(SYS_memfd_create, "weston-capture", MFD_CLOEXEC);
	if (fd >= 0 && ftruncate(fd, (off_t)size) == 0)
		return fd;
	if (fd >= 0)
		close(fd);

	char name[] = "/weston-capture-XXXXXX";
	fd = shm_open(name, O_RDWR | O_CREAT | O_EXCL, 0600);
	if (fd < 0)
		return -1;
	shm_unlink(name);
	if (ftruncate(fd, (off_t)size) < 0) {
		close(fd);
		return -1;
	}
	return fd;
}

static void
output_geometry(void *data, struct wl_output *output, int32_t x, int32_t y,
		       int32_t physical_width, int32_t physical_height,
		       int32_t subpixel, const char *make, const char *model,
		       int32_t transform)
{
	(void)data;
	(void)output;
	(void)x;
	(void)y;
	(void)physical_width;
	(void)physical_height;
	(void)subpixel;
	(void)make;
	(void)model;
	(void)transform;
}

static void
output_mode(void *data, struct wl_output *output, uint32_t flags,
		    int32_t width, int32_t height, int32_t refresh)
{
	struct capture *capture = data;
	(void)output;
	(void)refresh;
	if (flags & WL_OUTPUT_MODE_CURRENT) {
		capture->width = width;
		capture->height = height;
	}
}

static void
output_done(void *data, struct wl_output *output)
{
	(void)data;
	(void)output;
}

static void
output_scale(void *data, struct wl_output *output, int32_t factor)
{
	(void)data;
	(void)output;
	(void)factor;
}

static const struct wl_output_listener output_listener = {
	.geometry = output_geometry,
	.mode = output_mode,
	.done = output_done,
	.scale = output_scale,
};

static void
screenshot_done(void *data, struct weston_screenshooter *screenshooter)
{
	struct capture *capture = data;
	(void)screenshooter;
	capture->done = 1;
}

static const struct weston_screenshooter_listener screenshooter_listener = {
	.done = screenshot_done,
};

static void
registry_global(void *data, struct wl_registry *registry, uint32_t name,
		const char *interface, uint32_t version)
{
	struct capture *capture = data;
	(void)version;
	if (strcmp(interface, "wl_shm") == 0 && capture->shm == NULL) {
		capture->shm = wl_registry_bind(registry, name, &wl_shm_interface, 1);
	} else if (strcmp(interface, "wl_output") == 0 && capture->output == NULL) {
		capture->output = wl_registry_bind(registry, name, &wl_output_interface, 1);
		wl_output_add_listener(capture->output, &output_listener, capture);
	} else if (strcmp(interface, "weston_screenshooter") == 0 &&
		   capture->screenshooter == NULL) {
		capture->screenshooter = wl_registry_bind(
			registry, name, &weston_screenshooter_interface, 1);
	}
}

static void
registry_global_remove(void *data, struct wl_registry *registry, uint32_t name)
{
	(void)data;
	(void)registry;
	(void)name;
}

static const struct wl_registry_listener registry_listener = {
	.global = registry_global,
	.global_remove = registry_global_remove,
};

static int
create_buffer(struct capture *capture)
{
	int fd;
	int stride = capture->width * 4;
	capture->size = (size_t)stride * (size_t)capture->height;
	fd = create_anonymous_file(capture->size);
	if (fd < 0) {
		fprintf(stderr, "create capture buffer: %s\n", strerror(errno));
		return -1;
	}

	capture->pixels = mmap(NULL, capture->size, PROT_READ | PROT_WRITE,
			       MAP_SHARED, fd, 0);
	if (capture->pixels == MAP_FAILED) {
		fprintf(stderr, "map capture buffer: %s\n", strerror(errno));
		close(fd);
		return -1;
	}

	struct wl_shm_pool *pool = wl_shm_create_pool(capture->shm, fd,
						      (int)capture->size);
	close(fd);
	if (pool == NULL)
		return -1;
	capture->buffer = wl_shm_pool_create_buffer(
		pool, 0, capture->width, capture->height, stride,
		WL_SHM_FORMAT_XRGB8888);
	wl_shm_pool_destroy(pool);
	return capture->buffer == NULL ? -1 : 0;
}

static int
write_all(int fd, const void *data, size_t size)
{
	const char *bytes = data;
	while (size > 0) {
		ssize_t written = write(fd, bytes, size);
		if (written < 0) {
			if (errno == EINTR)
				continue;
			return -1;
		}
		bytes += written;
		size -= (size_t)written;
	}
	return 0;
}

static int
parse_framerate(int argc, char **argv)
{
	for (int i = 1; i + 1 < argc; i++) {
		if (strcmp(argv[i], "--framerate") == 0) {
			int fps = atoi(argv[i + 1]);
			if (fps > 0 && fps <= 120)
				return fps;
		}
	}
	return 25;
}

int
main(int argc, char **argv)
{
	struct capture capture = {};
	struct sigaction action = {
		.sa_handler = handle_signal,
	};
	int fps = parse_framerate(argc, argv);
	struct timespec interval = {
		.tv_sec = 0,
		.tv_nsec = 1000000000L / fps,
	};

	sigemptyset(&action.sa_mask);
	sigaction(SIGINT, &action, NULL);
	sigaction(SIGTERM, &action, NULL);

	capture.display = wl_display_connect(NULL);
	if (capture.display == NULL) {
		fprintf(stderr, "connect to Wayland display: %s\n", strerror(errno));
		return 1;
	}

	struct wl_registry *registry = wl_display_get_registry(capture.display);
	wl_registry_add_listener(registry, &registry_listener, &capture);
	if (wl_display_roundtrip(capture.display) < 0 ||
	    wl_display_roundtrip(capture.display) < 0) {
		fprintf(stderr, "discover Wayland globals: %s\n", strerror(errno));
		return 1;
	}
	if (capture.shm == NULL || capture.output == NULL ||
	    capture.screenshooter == NULL || capture.width <= 0 ||
	    capture.height <= 0) {
		fprintf(stderr, "Wayland screenshooter or output is unavailable\n");
		return 1;
	}
	if (create_buffer(&capture) < 0)
		return 1;
	weston_screenshooter_add_listener(capture.screenshooter,
					   &screenshooter_listener, &capture);

	while (!stopping) {
		capture.done = 0;
		weston_screenshooter_shoot(capture.screenshooter, capture.output,
					   capture.buffer);
		if (wl_display_flush(capture.display) < 0 && errno != EAGAIN)
			break;
		while (!capture.done && !stopping) {
			if (wl_display_dispatch(capture.display) < 0) {
				stopping = 1;
				break;
			}
		}
		if (stopping)
			break;
		if (write_all(STDOUT_FILENO, capture.pixels, capture.size) < 0)
			break;
		nanosleep(&interval, NULL);
	}

	if (capture.pixels != NULL && capture.pixels != MAP_FAILED)
		munmap(capture.pixels, capture.size);
	if (capture.buffer != NULL)
		wl_buffer_destroy(capture.buffer);
	if (capture.screenshooter != NULL)
		weston_screenshooter_destroy(capture.screenshooter);
	if (capture.output != NULL)
		wl_output_destroy(capture.output);
	if (capture.shm != NULL)
		wl_shm_destroy(capture.shm);
	if (registry != NULL)
		wl_registry_destroy(registry);
	wl_display_disconnect(capture.display);
	return 0;
}
