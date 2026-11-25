// Controlled capture with the asynchronous scanner's kernel-read implementation.
#define main idle_scan_main
#include "idle-scan.c"
#undef main

#define REASON_SHIFT 56
#define REASON_MASK 7
#define ABSENT_AT_ARM 64
#define ABSENT_AT_END 128

static void capture_inventory(struct observer *o, struct snapshot *snapshot)
{
    check_identity(o);
    inventory(o, snapshot);
    for (size_t i = 0; i < o->ram_pages; i++) {
        uint64_t page = snapshot->pages[i], pfn = page & PFN_MASK;
        if (page && !(page & ELIGIBLE))
            snapshot->pages[i] |= (uint64_t)(pfn ? o->reasons[pfn] + 1 : 1) << REASON_SHIFT;
    }
    check_identity(o);
}

static unsigned char evidence(uint64_t prior, uint64_t mid, uint64_t final,
                              const uint64_t *bits)
{
    if (!prior)
        return ABSENT_AT_ARM | (final ? 0 : ABSENT_AT_END) | (mid || final ? 3 : 0);
    if (!mid || !final)
        return (final ? 0 : ABSENT_AT_END) | 4;
    uint64_t pfn = prior & PFN_MASK;
    if (pfn != (mid & PFN_MASK) || pfn != (final & PFN_MASK))
        return 5;
    for (size_t i = 0; i < 3; i++) {
        uint64_t page = (uint64_t[]){prior, mid, final}[i];
        uint64_t reason = (page >> REASON_SHIFT) & REASON_MASK;
        if (reason)
            return 5 + reason;
    }
    return marked(bits, pfn) ? 2 : 1;
}

static void export_window(const struct observer *o, const struct snapshot *prior,
                          const struct snapshot *mid, const struct snapshot *final,
                          const char *directory, size_t window, unsigned char *codes)
{
    uint64_t begin = ns(CLOCK_MONOTONIC);
    uint64_t counts[11] = {0}, absent_arm = 0, absent_end = 0;
    for (size_t i = 0; i < o->ram_pages; i++) {
        unsigned char code = evidence(prior->pages[i], mid->pages[i], final->pages[i], o->idle_bits);
        codes[i] = code;
        counts[code & 63]++;
        absent_arm += !!(code & ABSENT_AT_ARM);
        absent_end += !!(code & ABSENT_AT_END);
    }
    char *path;
    if (asprintf(&path, "%s/idle-%02zu.u8", directory, window) < 0)
        err(1, "idle window output path");
    FILE *out = fopen(path, "wbx");
    if (!out || fwrite(codes, 1, o->ram_pages, out) != o->ram_pages || fclose(out))
        err(1, "%s", path);
    free(path);
    printf("{\"kind\":\"exported\",\"window\":%zu,\"export_begin_ns\":%" PRIu64
           ",\"export_end_ns\":%" PRIu64 ",\"absent_at_arm\":%" PRIu64
           ",\"absent_at_end\":%" PRIu64 ",\"code_counts\":[%" PRIu64,
           window, begin, ns(CLOCK_MONOTONIC), absent_arm, absent_end, counts[0]);
    for (size_t i = 1; i < sizeof(counts) / sizeof(counts[0]); i++)
        printf(",%" PRIu64, counts[i]);
    puts("]}");
}

static void boundaries(struct observer *o, const char *directory)
{
    struct snapshot prior = {.pages = resize(NULL, o->ram_pages, sizeof(uint64_t))};
    struct snapshot mid = {.pages = resize(NULL, o->ram_pages, sizeof(uint64_t))};
    struct snapshot final = {.pages = resize(NULL, o->ram_pages, sizeof(uint64_t))};
    struct snapshot next = {.pages = resize(NULL, o->ram_pages, sizeof(uint64_t))};
    unsigned char *codes = resize(NULL, o->ram_pages, 1);
    size_t window = 0;
    bool armed = false, captured = false;
    char command[16];
    printf("{\"kind\":\"ready\",\"scan_threads\":%zu}\n", o->scan_threads);
    while (fgets(command, sizeof(command), stdin)) {
        uint64_t begin = ns(CLOCK_MONOTONIC), cpu = ns(CLOCK_PROCESS_CPUTIME_ID);
        if (!strcmp(command, "capture\n") && armed && !captured) {
            capture_inventory(o, &mid);
            prepare_bitmap(o, &prior);
            uint64_t bitmap_begin = ns(CLOCK_MONOTONIC);
            transfer_bitmap(o, false);
            uint64_t bitmap_end = ns(CLOCK_MONOTONIC);
            capture_inventory(o, &final);
            printf("{\"kind\":\"captured\",\"scan_threads\":%zu,\"scan_begin_ns\":%" PRIu64
                   ",\"scan_end_ns\":%" PRIu64 ",\"scan_cpu_ns\":%" PRIu64
                   ",\"bitmap_read_begin_ns\":%" PRIu64 ",\"bitmap_read_end_ns\":%" PRIu64
                   ",\"resident_pages_at_end\":%" PRIu64 "}\n",
                   o->scan_threads, begin, ns(CLOCK_MONOTONIC), ns(CLOCK_PROCESS_CPUTIME_ID) - cpu,
                   bitmap_begin, bitmap_end, final.resident);
            armed = false;
            captured = true;
        } else if (!strcmp(command, "arm\n") && !armed) {
            struct snapshot *snapshot = captured ? &next : &prior;
            capture_inventory(o, snapshot);
            prepare_bitmap(o, snapshot);
            uint64_t bitmap_begin = ns(CLOCK_MONOTONIC);
            transfer_bitmap(o, true);
            uint64_t bitmap_end = ns(CLOCK_MONOTONIC);
            check_identity(o);
            printf("{\"kind\":\"armed\",\"scan_threads\":%zu,\"arm_begin_ns\":%" PRIu64
                   ",\"arm_end_ns\":%" PRIu64 ",\"arm_cpu_ns\":%" PRIu64
                   ",\"bitmap_write_begin_ns\":%" PRIu64 ",\"bitmap_write_end_ns\":%" PRIu64
                   ",\"resident_pages_at_arm\":%" PRIu64 ",\"eligible_pages_at_arm\":%" PRIu64 "}\n",
                   o->scan_threads, begin, ns(CLOCK_MONOTONIC), ns(CLOCK_PROCESS_CPUTIME_ID) - cpu,
                   bitmap_begin, bitmap_end, snapshot->resident, snapshot->eligible);
            armed = true;
        } else if (!strcmp(command, "export\n") && armed && captured) {
            export_window(o, &prior, &mid, &final, directory, ++window, codes);
            struct snapshot old = prior;
            prior = next;
            next = old;
            captured = false;
        } else {
            errx(1, "unexpected native boundary command: %s", command);
        }
    }
    if (captured || ferror(stdin) || fflush(stdout) == EOF || ferror(stdout))
        errx(1, "native boundary stream ended during capture");
}

int main(int argc, char **argv)
{
    if (argc == 2 && !strcmp(argv[1], "--self-test")) {
        self_test();
        uint64_t present = PRESENT | ELIGIBLE, a = present | 1, b = present | 2;
        uint64_t bits[] = {UINT64_C(1) << 1};
        assert(evidence(a, a, a, bits) == 2 && evidence(b, b, b, bits) == 1);
        assert(evidence(0, b, 0, bits) == (ABSENT_AT_ARM | ABSENT_AT_END | 3));
        assert(evidence(a, 0, b, bits) == 4 && evidence(a, b, a, bits) == 5);
        assert(evidence(a, a, PRESENT | 1 | (UINT64_C(3) << REASON_SHIFT), bits) == 8);
        return 0;
    }
    if (argc < 6)
        errx(1, "usage: native-deep-scan OUTPUT PID START_TICKS HVA SIZE [HVA SIZE ...]");
    struct observer o = open_observer(argc - 1, argv + 1);
    if (setvbuf(stdout, NULL, _IOLBF, 0))
        err(1, "setvbuf");
    start_scan_threads(&o, requested_scan_threads(), IDLE_BITMAP);
    boundaries(&o, argv[1]);
    stop_scan_threads(&o);
    return 0;
}
