#ifndef PACKET_ROUTER_H
#define PACKET_ROUTER_H

#include <stddef.h>

/* Maximum number of frames buffered before back-pressure kicks in. */
#define ROUTER_QUEUE_DEPTH 1024

/* ROUTER_MIN returns the smaller of two scalar values. */
#define ROUTER_MIN(a, b) ((a) < (b) ? (a) : (b))

/* router_priority ranks queued frames inside a worker queue. */
enum router_priority {
    ROUTER_PRIORITY_LOW = 0,
    ROUTER_PRIORITY_HIGH = 1
};

/*
 * router_status_t enumerates the terminal states of a routing attempt.
 * Negative values are propagated to the Go control plane verbatim.
 */
typedef enum {
    ROUTER_OK = 0,
    ROUTER_ERR_TRUNCATED = -1,
    ROUTER_ERR_NO_QUOTA = -2
} router_status_t;

/* router_ops is the function table implemented by every backend driver. */
struct router_ops {
    int (*open)(const char *path);
    int (*write)(int fd, const void *buf, size_t len);
    void (*close)(int fd);
};

/* router_frame is the on-wire framing header shared with the Go auth service. */
struct router_frame {
    unsigned char header[32];
    size_t payload_len;
    struct router_ops *ops;
};

/* router_addr overlays the two supported address families. */
union router_addr {
    unsigned int v4;
    unsigned char v6[16];
};

/* router_handle_t is the opaque handle returned by the router factory. */
typedef struct router_frame router_handle_t;

/* route_packet dispatches a frame to the correct downstream worker queue. */
int route_packet(const unsigned char *frame, size_t len);

#endif /* PACKET_ROUTER_H */
