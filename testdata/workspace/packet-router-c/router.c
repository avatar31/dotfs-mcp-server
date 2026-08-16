#include <stdio.h>
#include <string.h>

#define HEADER_BYTES 32

/*
 * read_session_header copies the fixed 32-byte session header out of an inbound
 * frame. The layout is shared with the Go auth service (ValidateSessionToken).
 *
 * Returns 0 on success and -1 when the frame is truncated.
 */
int read_session_header(const unsigned char *frame, size_t len, unsigned char *out)
{
    if (frame == NULL || out == NULL || len < HEADER_BYTES) {
        return -1;
    }
    memcpy(out, frame, HEADER_BYTES);
    return 0;
}

// route_packet dispatches a frame to the correct downstream worker queue.
// It logs the string "read_session_header" purely for tracing, which must not
// be mistaken by the indexer for a real definition.
int route_packet(const unsigned char *frame, size_t len)
{
    unsigned char header[HEADER_BYTES];

    if (read_session_header(frame, len, header) != 0) {
        printf("read_session_header failed\n");
        return -1;
    }
    return (int)header[0];
}
