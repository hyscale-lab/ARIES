// Async host page-idle observation of every eligible page in validated KVM RAM slots.
#define _GNU_SOURCE
#include <assert.h>
#include <err.h>
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <linux/kernel-page-flags.h>
#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#ifdef _REENTRANT
#include <pthread.h>
#include <stdatomic.h>
#endif

#define PAGE_BYTES 4096
#define BITMAP_WORDS (PAGE_BYTES / sizeof(uint64_t))
#define METADATA_WORDS 8192
#define MAX_SCAN_THREADS 64
#define IDLE_BITMAP "/sys/kernel/mm/page_idle/bitmap"
#define NS_PER_SECOND UINT64_C(1000000000)
#define PRESENT (UINT64_C(1) << 63)
#define ELIGIBLE (UINT64_C(1) << 62)
#define PFN_MASK ((UINT64_C(1) << 55) - 1)
#define FLAG(bit) (UINT64_C(1) << (bit))
#define COMPOUND (FLAG(KPF_COMPOUND_HEAD) | FLAG(KPF_COMPOUND_TAIL) | FLAG(KPF_HUGE) | FLAG(KPF_THP))
#define SHARED (FLAG(KPF_KSM) | FLAG(KPF_ZERO_PAGE))

struct slot {
    uint64_t first_page;
    size_t pages;
};

struct snapshot {
    uint64_t *pages;
    uint64_t resident, eligible, masked, compound, shared, non_lru, nonexclusive;
};

struct scan_range {
    size_t start, count;
    uint64_t *words;
};

struct scan_pool;

struct observer {
    int pagemap, flags, count, idle, stat;
    uint64_t pid, start_ticks;
    struct slot *slots;
    size_t slot_count, ram_pages;
    size_t max_pfn, words, capacity;
    uint64_t *target, *idle_bits;
    unsigned char *reasons;
    struct scan_range *ranges;
    size_t range_count, range_capacity, scan_threads;
    struct scan_pool *pool;
};

enum eligibility { EXCLUSIVE, COMPLEX, SHARED_OR_ZERO, NON_LRU, NONEXCLUSIVE };

struct window {
    uint64_t accessed, idle, new_resident, lost, moved, ineligible;
};

struct arm_times {
    uint64_t begin, end, bitmap_begin, bitmap_end, cpu;
};

static volatile sig_atomic_t stopping;

static void stop(int signal_number)
{
    (void)signal_number;
    stopping = 1;
}

static uint64_t ns(clockid_t clock)
{
    struct timespec t;
    if (clock_gettime(clock, &t))
        err(1, "clock_gettime");
    return (uint64_t)t.tv_sec * NS_PER_SECOND + t.tv_nsec;
}

static void sleep_until(uint64_t deadline)
{
    const struct timespec t = {
        .tv_sec = deadline / NS_PER_SECOND,
        .tv_nsec = deadline % NS_PER_SECOND,
    };
    int status = clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &t, NULL);
    if (status && status != EINTR)
        errx(1, "clock_nanosleep: %s", strerror(status));
}

static int open_fd(const char *path, int flags)
{
    int fd = open(path, flags | O_CLOEXEC);
    if (fd < 0)
        err(1, "%s", path);
    return fd;
}

static void read_words(int fd, uint64_t *out, size_t start, size_t count)
{
    size_t bytes = count * sizeof(*out);
    ssize_t read_bytes;
    do {
        read_bytes = pread(fd, out, bytes, start * sizeof(*out));
    } while (read_bytes == -1 && errno == EINTR);
    if (read_bytes == -1)
        err(1, "read fd %d at word %zu", fd, start);
    if ((size_t)read_bytes != bytes)
        errx(1, "short read fd %d at word %zu: %zd of %zu bytes", fd, start, read_bytes, bytes);
}

static void write_words(int fd, const uint64_t *data, size_t start, size_t count)
{
    size_t bytes = count * sizeof(*data);
    ssize_t written;
    do {
        written = pwrite(fd, data, bytes, start * sizeof(*data));
    } while (written == -1 && errno == EINTR);
    if (written == -1)
        err(1, "write idle bitmap at word %zu", start);
    if ((size_t)written != bytes)
        errx(1, "short idle bitmap write at word %zu: %zd of %zu bytes", start, written, bytes);
}

static void check_identity(const struct observer *o)
{
    char data[4096], *cursor;
    ssize_t bytes;
    do {
        bytes = pread(o->stat, data, sizeof(data) - 1, 0);
    } while (bytes == -1 && errno == EINTR);
    if (bytes <= 0)
        errx(1, "lost Firecracker process %" PRIu64, o->pid);
    data[bytes] = '\0';
    cursor = strrchr(data, ')');
    for (int field = 3; cursor && field <= 22; field++)
        cursor = strchr(cursor + 1, ' ');
    if (!cursor || strtoull(cursor, NULL, 10) != o->start_ticks)
        errx(1, "Firecracker identity changed for pid %" PRIu64, o->pid);
}

static void *resize(void *ptr, size_t count, size_t bytes)
{
    void *next = reallocarray(ptr, count, bytes);
    if (!next)
        err(1, "allocating scan buffers");
    return next;
}

typedef void (*scan_fn)(struct observer *, const struct scan_range *, int);

#ifdef _REENTRANT
struct scan_worker {
    struct scan_pool *pool;
    pthread_t thread;
    int idle;
};

struct scan_pool {
    struct observer *observer;
    pthread_mutex_t lock;
    pthread_cond_t changed;
    struct scan_worker workers[MAX_SCAN_THREADS - 1];
    size_t generation, pending;
    _Atomic size_t next;
    scan_fn run;
    bool stopping;
};

static void run_ranges(struct scan_pool *pool, int idle)
{
    struct observer *o = pool->observer;
    for (;;) {
        size_t index = atomic_fetch_add_explicit(&pool->next, 1, memory_order_relaxed);
        if (index >= o->range_count)
            return;
        pool->run(o, &o->ranges[index], idle);
    }
}

static void *scan_worker(void *arg)
{
    const struct scan_worker *worker = arg;
    struct scan_pool *pool = worker->pool;
    size_t generation = 0;
    pthread_mutex_lock(&pool->lock);
    for (;;) {
        while (pool->generation == generation && !pool->stopping)
            pthread_cond_wait(&pool->changed, &pool->lock);
        if (pool->stopping)
            break;
        generation = pool->generation;
        pthread_mutex_unlock(&pool->lock);
        run_ranges(pool, worker->idle);
        pthread_mutex_lock(&pool->lock);
        if (!--pool->pending)
            pthread_cond_broadcast(&pool->changed);
    }
    pthread_mutex_unlock(&pool->lock);
    return NULL;
}
#endif

static void start_scan_threads(struct observer *o, size_t requested, const char *bitmap)
{
    o->scan_threads = 1;
#ifdef _REENTRANT
    if (requested == 1)
        return;
    struct scan_pool *pool = calloc(1, sizeof(*pool));
    if (!pool)
        err(1, "allocating scan workers");
    o->pool = pool;
    pool->observer = o;
    atomic_init(&pool->next, 0);
    int status = pthread_mutex_init(&pool->lock, NULL);
    if (!status)
        status = pthread_cond_init(&pool->changed, NULL);
    if (status)
        errx(1, "initializing scan workers: %s", strerror(status));

    sigset_t mask, old;
    sigemptyset(&mask);
    sigaddset(&mask, SIGINT);
    sigaddset(&mask, SIGTERM);
    status = pthread_sigmask(SIG_BLOCK, &mask, &old);
    if (status)
        errx(1, "blocking scan worker signals: %s", strerror(status));
    while (o->scan_threads < requested) {
        struct scan_worker *worker = &pool->workers[o->scan_threads - 1];
        worker->pool = pool;
        // Kernfs serializes I/O within a single open file description.
        worker->idle = open(bitmap, O_RDWR | O_CLOEXEC);
        if (worker->idle < 0) {
            warn("opening idle bitmap for scan worker");
            break;
        }
        status = pthread_create(&worker->thread, NULL, scan_worker, worker);
        if (status) {
            warnx("starting scan worker: %s", strerror(status));
            close(worker->idle);
            break;
        }
        o->scan_threads++;
    }
    status = pthread_sigmask(SIG_SETMASK, &old, NULL);
    if (status)
        errx(1, "restoring scan signal mask: %s", strerror(status));
#else
    (void)bitmap;
    if (requested > 1)
        warnx("parallel scanning requires a build with -pthread");
#endif
}

static void stop_scan_threads(struct observer *o)
{
#ifdef _REENTRANT
    struct scan_pool *pool = o->pool;
    if (pool) {
        pthread_mutex_lock(&pool->lock);
        pool->stopping = true;
        pthread_cond_broadcast(&pool->changed);
        pthread_mutex_unlock(&pool->lock);
        for (size_t i = 0; i + 1 < o->scan_threads; i++) {
            pthread_join(pool->workers[i].thread, NULL);
            close(pool->workers[i].idle);
        }
        pthread_cond_destroy(&pool->changed);
        pthread_mutex_destroy(&pool->lock);
        free(pool);
    }
#endif
    free(o->ranges);
}

static void add_range(struct observer *o, size_t start, size_t count, uint64_t *words)
{
    if (o->range_count == o->range_capacity) {
        o->range_capacity = o->range_capacity ? o->range_capacity * 2 : 256;
        o->ranges = resize(o->ranges, o->range_capacity, sizeof(*o->ranges));
    }
    o->ranges[o->range_count++] = (struct scan_range){start, count, words};
}

static void scan_ranges(struct observer *o, scan_fn run)
{
#ifdef _REENTRANT
    if (o->scan_threads > 1 && o->range_count > 1) {
        struct scan_pool *pool = o->pool;
        pthread_mutex_lock(&pool->lock);
        pool->run = run;
        atomic_store_explicit(&pool->next, 0, memory_order_relaxed);
        pool->pending = o->scan_threads - 1;
        pool->generation++;
        pthread_cond_broadcast(&pool->changed);
        pthread_mutex_unlock(&pool->lock);

        run_ranges(pool, o->idle);

        pthread_mutex_lock(&pool->lock);
        while (pool->pending)
            pthread_cond_wait(&pool->changed, &pool->lock);
        pthread_mutex_unlock(&pool->lock);
    } else
#endif
    {
        for (size_t i = 0; i < o->range_count; i++)
            run(o, &o->ranges[i], o->idle);
    }
    o->range_count = 0;
}

static bool marked(const uint64_t *bits, uint64_t pfn)
{
    return bits[pfn / 64] & (UINT64_C(1) << (pfn % 64));
}

static void mark(uint64_t *bits, uint64_t pfn)
{
    bits[pfn / 64] |= UINT64_C(1) << (pfn % 64);
}

static enum eligibility eligibility(uint64_t flags, uint64_t count)
{
    if (flags & COMPOUND)
        return COMPLEX;
    if (flags & SHARED)
        return SHARED_OR_ZERO;
    if (!(flags & FLAG(KPF_LRU)))
        return NON_LRU;
    if (count != 1)
        return NONEXCLUSIVE;
    return EXCLUSIVE;
}

static void read_pagemap(struct observer *o, const struct scan_range *range, int idle)
{
    (void)idle;
    read_words(o->pagemap, range->words, range->start, range->count);
}

static void read_metadata(struct observer *o, const struct scan_range *range, int idle)
{
    (void)idle;
    uint64_t flags[METADATA_WORDS], counts[METADATA_WORDS];
    read_words(o->flags, flags, range->start, range->count);
    read_words(o->count, counts, range->start, range->count);
    for (size_t i = 0; i < range->count; i++)
        o->reasons[range->start + i] = eligibility(flags[i], counts[i]);
}

static void inventory(struct observer *o, struct snapshot *snapshot)
{
    uint64_t *pages = snapshot->pages;
    *snapshot = (struct snapshot){.pages = pages};
    size_t cursor = 0, max_pfn = 0;
    for (size_t i = 0; i < o->slot_count; i++) {
        const struct slot slot = o->slots[i];
        for (size_t offset = 0; offset < slot.pages; offset += METADATA_WORDS) {
            size_t count = slot.pages - offset;
            if (count > METADATA_WORDS)
                count = METADATA_WORDS;
            add_range(o, slot.first_page + offset, count, pages + cursor);
            cursor += count;
        }
    }
    scan_ranges(o, read_pagemap);
    for (size_t i = 0; i < cursor; i++) {
        if (!(pages[i] & PRESENT)) {
            pages[i] = 0;
            continue;
        }
        pages[i] &= PRESENT | PFN_MASK;
        uint64_t pfn = pages[i] & PFN_MASK;
        snapshot->resident++;
        if (pfn > max_pfn)
            max_pfn = pfn;
    }
    if (max_pfn > o->max_pfn)
        o->max_pfn = max_pfn;
    o->words = o->max_pfn / 64 + 1;
    if (o->words > o->capacity) {
        size_t old_capacity = o->capacity;
        o->capacity = o->words;
        o->target = resize(o->target, o->capacity, sizeof(*o->target));
        o->idle_bits = resize(o->idle_bits, o->capacity, sizeof(*o->idle_bits));
        memset(o->idle_bits + old_capacity, 0, (o->capacity - old_capacity) * sizeof(*o->idle_bits));
        o->reasons = resize(o->reasons, o->capacity * 64, sizeof(*o->reasons));
    }
    memset(o->target, 0, o->words * sizeof(*o->target));
    for (size_t i = 0; i < cursor; i++) {
        uint64_t pfn = pages[i] & PFN_MASK;
        if (pfn)
            mark(o->target, pfn);
    }

    for (size_t word = 0; word <= max_pfn / 64;) {
        if (!o->target[word]) {
            word++;
            continue;
        }
        size_t first = word;
        size_t last = word;
        size_t end = first + METADATA_WORDS / 64;
        if (end > max_pfn / 64 + 1)
            end = max_pfn / 64 + 1;
        // Merge nearby requested metadata words to avoid small /proc reads.
        for (word++; word < end && word - last <= 4; word++) {
            if (o->target[word])
                last = word;
        }
        size_t first_pfn = first * 64 + __builtin_ctzll(o->target[first]);
        size_t last_pfn = last * 64 + 63 - __builtin_clzll(o->target[last]);
        add_range(o, first_pfn, last_pfn - first_pfn + 1, NULL);
    }
    scan_ranges(o, read_metadata);
    for (size_t i = 0; i < cursor; i++) {
        if (!pages[i])
            continue;
        uint64_t pfn = pages[i] & PFN_MASK;
        if (!pfn) {
            snapshot->masked++;
            continue;
        }
        switch (o->reasons[pfn]) {
        case EXCLUSIVE:
            pages[i] |= ELIGIBLE;
            snapshot->eligible++;
            break;
        case COMPLEX:
            snapshot->compound++;
            break;
        case SHARED_OR_ZERO:
            snapshot->shared++;
            break;
        case NON_LRU:
            snapshot->non_lru++;
            break;
        case NONEXCLUSIVE:
            snapshot->nonexclusive++;
            break;
        }
    }
}

static void prepare_bitmap(const struct observer *o, const struct snapshot *snapshot)
{
    memset(o->target, 0, o->words * sizeof(*o->target));
    for (size_t i = 0; i < o->ram_pages; i++) {
        uint64_t page = snapshot->pages[i];
        if (page & ELIGIBLE)
            mark(o->target, page & PFN_MASK);
    }
}

static void read_bitmap(struct observer *o, const struct scan_range *range, int idle)
{
    (void)o;
    read_words(idle, range->words, range->start, range->count);
}

static void write_bitmap(struct observer *o, const struct scan_range *range, int idle)
{
    (void)o;
    write_words(idle, range->words, range->start, range->count);
}

static void transfer_bitmap(struct observer *o, bool write)
{
    uint64_t *words = write ? o->target : o->idle_bits;
    for (size_t i = 0; i < o->words;) {
        if (!o->target[i]) {
            i++;
            continue;
        }
        size_t start = i++;
        while (i < o->words && o->target[i] && i - start < BITMAP_WORDS)
            i++;
        add_range(o, start, i - start, words + start);
    }
    scan_ranges(o, write ? write_bitmap : read_bitmap);
}

static struct window classify(size_t count, const struct snapshot *prior,
                              const struct snapshot *mid, const struct snapshot *final,
                              const uint64_t *idle_bits)
{
    struct window result = {0};
    for (size_t i = 0; i < count; i++) {
        uint64_t a = prior->pages[i], m = mid->pages[i], b = final->pages[i];
        if (a == m && m == b && (a & ELIGIBLE)) {
            if (marked(idle_bits, a & PFN_MASK))
                result.idle++;
            else
                result.accessed++;
        } else if (!a) {
            result.new_resident += !!b;
        } else if (!m || !b) {
            result.lost++;
        } else if ((a & PFN_MASK) != (m & PFN_MASK) || (a & PFN_MASK) != (b & PFN_MASK)) {
            result.moved++;
        } else {
            result.ineligible++;
        }
    }
    return result;
}

static void fraction(const char *name, uint64_t count, uint64_t total)
{
    printf(",\"%s\":", name);
    if (total)
        printf("%.9f", (double)count / total);
    else
        printf("null");
}

static void emit_snapshot(const struct observer *o, uint64_t tick, uint64_t scheduled,
                          const struct snapshot *current, struct arm_times arm)
{
    printf("{\"status\":\"%s\",\"tick\":%" PRIu64 ",\"scheduled_ns\":%" PRIu64
           ",\"pid\":%" PRIu64 ",\"start_ticks\":%" PRIu64 ",\"ram_pages\":%zu,\"scan_threads\":%zu"
           ",\"resident_pages\":%" PRIu64 ",\"eligible_pages\":%" PRIu64
           ",\"masked_pfn_pages\":%" PRIu64 ",\"compound_pages\":%" PRIu64
           ",\"shared_or_zero_pages\":%" PRIu64 ",\"non_lru_pages\":%" PRIu64
           ",\"nonexclusive_pages\":%" PRIu64 ",\"arm_begin_ns\":%" PRIu64
           ",\"arm_end_ns\":%" PRIu64 ",\"bitmap_write_begin_ns\":%" PRIu64
           ",\"bitmap_write_end_ns\":%" PRIu64 ",\"arm_cpu_ns\":%" PRIu64,
           tick ? "observed" : "armed", tick, scheduled, o->pid, o->start_ticks, o->ram_pages, o->scan_threads,
           current->resident, current->eligible, current->masked, current->compound,
           current->shared, current->non_lru, current->nonexclusive, arm.begin,
           arm.end, arm.bitmap_begin, arm.bitmap_end, arm.cpu);
}

static void emit_window(const struct snapshot *prior, const struct snapshot *current,
                        struct window result, struct arm_times arm, uint64_t previous_tick,
                        uint64_t scan_begin, uint64_t scan_end, uint64_t scan_cpu,
                        uint64_t bitmap_read_begin, uint64_t bitmap_read_end, size_t ram_pages)
{
    uint64_t observed = result.accessed + result.idle;
    printf(",\"previous_tick\":%" PRIu64 ",\"previous_arm_begin_ns\":%" PRIu64
           ",\"previous_arm_end_ns\":%" PRIu64 ",\"previous_bitmap_write_begin_ns\":%" PRIu64
           ",\"previous_bitmap_write_end_ns\":%" PRIu64 ",\"scan_begin_ns\":%" PRIu64
           ",\"scan_end_ns\":%" PRIu64 ",\"scan_cpu_ns\":%" PRIu64
           ",\"bitmap_read_begin_ns\":%" PRIu64 ",\"bitmap_read_end_ns\":%" PRIu64
           ",\"prior_resident_pages\":%" PRIu64 ",\"prior_eligible_pages\":%" PRIu64
           ",\"observed_pages\":%" PRIu64 ",\"accessed_pages\":%" PRIu64
           ",\"idle_pages\":%" PRIu64 ",\"new_resident_pages\":%" PRIu64
           ",\"nonresident_endpoint_pages\":%" PRIu64 ",\"changed_pfn_pages\":%" PRIu64
           ",\"ineligible_endpoint_pages\":%" PRIu64,
           previous_tick, arm.begin, arm.end, arm.bitmap_begin, arm.bitmap_end,
           scan_begin, scan_end, scan_cpu, bitmap_read_begin, bitmap_read_end,
           prior->resident, prior->eligible, observed, result.accessed, result.idle,
           result.new_resident, result.lost, result.moved, result.ineligible);
    fraction("observed_fraction_of_ram", observed, ram_pages);
    fraction("observed_fraction_of_resident", observed, current->resident);
    fraction("observed_fraction_of_eligible", observed, current->eligible);
    fraction("observed_fraction_of_prior_eligible", observed, prior->eligible);
}

static void observe(struct observer *o)
{
    struct snapshot prior = {.pages = resize(NULL, o->ram_pages, sizeof(uint64_t))};
    struct snapshot mid = {.pages = resize(NULL, o->ram_pages, sizeof(uint64_t))};
    struct snapshot current = {.pages = resize(NULL, o->ram_pages, sizeof(uint64_t))};
    struct arm_times previous_arm = {0};
    uint64_t previous_tick = 0, missed = 0;
    uint64_t tick = 0, scheduled = ns(CLOCK_MONOTONIC);

    for (; !stopping; tick++, scheduled += NS_PER_SECOND) {
        sleep_until(scheduled);
        if (stopping)
            break;
        uint64_t begin = ns(CLOCK_MONOTONIC);
        if (tick && begin - scheduled > NS_PER_SECOND / 2) {
            printf("{\"status\":\"missed_tick\",\"tick\":%" PRIu64 ",\"scheduled_ns\":%" PRIu64
                   ",\"noticed_ns\":%" PRIu64 "}\n", tick, scheduled, begin);
            missed++;
            continue;
        }
        uint64_t cpu_begin = ns(CLOCK_PROCESS_CPUTIME_ID);
        uint64_t bitmap_read_begin = 0, bitmap_read_end = 0;
        struct window result = {0};
        check_identity(o);
        if (tick) {
            inventory(o, &mid);
            prepare_bitmap(o, &prior);
            bitmap_read_begin = ns(CLOCK_MONOTONIC);
            transfer_bitmap(o, false);
            bitmap_read_end = ns(CLOCK_MONOTONIC);
        }
        inventory(o, &current);
        if (tick)
            result = classify(o->ram_pages, &prior, &mid, &current, o->idle_bits);
        uint64_t scan_end = ns(CLOCK_MONOTONIC);
        uint64_t scan_cpu_end = ns(CLOCK_PROCESS_CPUTIME_ID);

        struct arm_times arm = {.begin = tick ? scan_end : begin};
        prepare_bitmap(o, &current);
        arm.bitmap_begin = ns(CLOCK_MONOTONIC);
        transfer_bitmap(o, true);
        arm.bitmap_end = ns(CLOCK_MONOTONIC);
        check_identity(o);
        arm.end = ns(CLOCK_MONOTONIC);
        arm.cpu = ns(CLOCK_PROCESS_CPUTIME_ID) - (tick ? scan_cpu_end : cpu_begin);

        emit_snapshot(o, tick, scheduled, &current, arm);
        if (tick)
            emit_window(&prior, &current, result, previous_arm, previous_tick,
                        begin, scan_end, scan_cpu_end - cpu_begin,
                        bitmap_read_begin, bitmap_read_end, o->ram_pages);
        puts("}");
        struct snapshot temp = prior;
        prior = current;
        current = temp;
        previous_arm = arm;
        previous_tick = tick;
    }
    printf("{\"status\":\"stopped\",\"timestamp_ns\":%" PRIu64 ",\"next_tick\":%" PRIu64
           ",\"missed_ticks\":%" PRIu64 ",\"observer_cpu_ns\":%" PRIu64 "}\n",
           ns(CLOCK_MONOTONIC), tick, missed, ns(CLOCK_PROCESS_CPUTIME_ID));
}

static uint64_t parse(const char *value)
{
    char *end;
    errno = 0;
    uint64_t number = strtoull(value, &end, 10);
    if (*value < '0' || *value > '9' || *end || errno)
        errx(1, "invalid numeric argument: %s", value);
    return number;
}

static size_t requested_scan_threads(void)
{
    const char *value = getenv("ZB_SCAN_THREADS");
    uint64_t threads = value ? parse(value) : 1;
    if (!threads || threads > MAX_SCAN_THREADS)
        errx(1, "ZB_SCAN_THREADS must be between 1 and %d", MAX_SCAN_THREADS);
    return threads;
}

static void test_scan_io(void)
{
    FILE *files[] = {tmpfile(), tmpfile(), tmpfile(), tmpfile()};
    for (size_t i = 0; i < sizeof(files) / sizeof(files[0]); i++) {
        if (!files[i])
            err(1, "creating scan fixture");
    }
    enum { FAR_PFN = 16389 };
    struct slot slots[] = {{2, 6}, {10, 3}};
    uint64_t entries[] = {PRESENT | 1, PRESENT | 2, PRESENT | 3, PRESENT | 4, PRESENT | 5,
                          PRESENT, 0, PRESENT | FAR_PFN, PRESENT | FAR_PFN};
    uint64_t pages[sizeof(entries) / sizeof(entries[0])];
    uint64_t flags[] = {FLAG(KPF_LRU), FLAG(KPF_THP), FLAG(KPF_KSM), 0, FLAG(KPF_LRU)};
    uint64_t counts[] = {1, 1, 1, 1, 2}, one = 1, lru = FLAG(KPF_LRU);
    struct observer o = {.pagemap = fileno(files[0]), .flags = fileno(files[1]),
                         .count = fileno(files[2]), .idle = fileno(files[3]),
                         .slot_count = 2, .slots = slots, .ram_pages = sizeof(pages) / sizeof(pages[0])};
    write_words(o.pagemap, entries, slots[0].first_page, slots[0].pages);
    write_words(o.pagemap, entries + slots[0].pages, slots[1].first_page, slots[1].pages);
    write_words(o.flags, flags, 1, 5);
    write_words(o.count, counts, 1, 5);
    write_words(o.flags, &lru, FAR_PFN, 1);
    write_words(o.count, &one, FAR_PFN, 1);
    char bitmap[64];
    snprintf(bitmap, sizeof(bitmap), "/proc/self/fd/%d", o.idle);
#ifdef _REENTRANT
    start_scan_threads(&o, 4, bitmap);
    assert(o.scan_threads == 4);
#else
    start_scan_threads(&o, 1, bitmap);
#endif
    struct snapshot snapshot = {.pages = pages};
    inventory(&o, &snapshot);
    entries[0] |= ELIGIBLE;
    entries[7] |= ELIGIBLE;
    entries[8] |= ELIGIBLE;
    assert(!memcmp(pages, entries, sizeof(pages)));
    assert(snapshot.resident == 8 && snapshot.eligible == 3 && snapshot.masked == 1);
    assert(snapshot.compound == 1 && snapshot.shared == 1 && snapshot.non_lru == 1 && snapshot.nonexclusive == 1);

    uint64_t target[BITMAP_WORDS + 3], bits[BITMAP_WORDS + 3], actual[BITMAP_WORDS + 3];
    for (size_t i = 0; i < BITMAP_WORDS + 3; i++) {
        target[i] = i == BITMAP_WORDS + 1 ? 0 : i + 1;
        bits[i] = UINT64_MAX;
    }
    free(o.target);
    free(o.idle_bits);
    free(o.reasons);
    o.target = target;
    o.idle_bits = bits;
    o.words = sizeof(target) / sizeof(target[0]);
    write_words(o.idle, bits, 0, o.words);
    transfer_bitmap(&o, true);
    transfer_bitmap(&o, false);
    read_words(o.idle, actual, 0, o.words);
    target[BITMAP_WORDS + 1] = UINT64_MAX;
    assert(!memcmp(bits, target, sizeof(bits)));
    assert(!memcmp(actual, target, sizeof(actual)));
    stop_scan_threads(&o);
    for (size_t i = 0; i < sizeof(files) / sizeof(files[0]); i++)
        fclose(files[i]);
}

static void self_test(void)
{
    uint64_t page = PRESENT | ELIGIBLE;
    uint64_t given[] = {page | 1, page | 2, 0, page | 3, page | 4, page | 5};
    uint64_t later[] = {page | 1, page | 2, page | 6, 0, page | 8, PRESENT | 5};
    struct snapshot prior = {.pages = given}, final = {.pages = later};
    uint64_t bits[] = {UINT64_C(1) << 1};
    struct window result = classify(6, &prior, &final, &final, bits);
    assert(result.idle == 1 && result.accessed == 1 && result.new_resident == 1);
    assert(result.lost == 1 && result.moved == 1 && result.ineligible == 1);
    assert(eligibility(FLAG(KPF_LRU), 1) == EXCLUSIVE);
    assert(eligibility(FLAG(KPF_LRU), 2) == NONEXCLUSIVE);
    assert(eligibility(FLAG(KPF_LRU) | FLAG(KPF_THP), 1) == COMPLEX);
    assert(eligibility(FLAG(KPF_LRU) | FLAG(KPF_KSM), 1) == SHARED_OR_ZERO);
    assert(eligibility(0, 1) == NON_LRU);
    test_scan_io();
}

static struct observer open_observer(int argc, char **argv)
{
    if (argc < 5 || !(argc % 2) || sysconf(_SC_PAGESIZE) != PAGE_BYTES)
        errx(1, "usage on a 4 KiB host: idle-scan PID START_TICKS HVA SIZE [HVA SIZE ...]");

    struct observer o = {.pid = parse(argv[1]), .start_ticks = parse(argv[2])};
    char path[64];
    snprintf(path, sizeof(path), "/proc/%" PRIu64 "/stat", o.pid);
    o.stat = open_fd(path, O_RDONLY);
    check_identity(&o);
    snprintf(path, sizeof(path), "/proc/%" PRIu64 "/pagemap", o.pid);
    o.pagemap = open_fd(path, O_RDONLY);
    o.flags = open_fd("/proc/kpageflags", O_RDONLY);
    o.count = open_fd("/proc/kpagecount", O_RDONLY);
    o.idle = open_fd(IDLE_BITMAP, O_RDWR);
    o.slot_count = (argc - 3) / 2;
    o.slots = resize(NULL, o.slot_count, sizeof(*o.slots));
    for (size_t i = 0; i < o.slot_count; i++) {
        uint64_t hva = parse(argv[i * 2 + 3]), bytes = parse(argv[i * 2 + 4]);
        if (!bytes || hva % PAGE_BYTES || bytes % PAGE_BYTES || UINT64_MAX - hva < bytes)
            errx(1, "invalid RAM slot");
        o.slots[i] = (struct slot){.first_page = hva / PAGE_BYTES, .pages = bytes / PAGE_BYTES};
        o.ram_pages += o.slots[i].pages;
    }
    return o;
}

int main(int argc, char **argv)
{
    if (argc == 2 && !strcmp(argv[1], "--self-test")) {
        self_test();
        return 0;
    }

    struct observer o = open_observer(argc, argv);
    struct sigaction action = {.sa_handler = stop};
    if (sigemptyset(&action.sa_mask) || sigaction(SIGTERM, &action, NULL) ||
        sigaction(SIGINT, &action, NULL))
        err(1, "sigaction");
    if (setvbuf(stdout, NULL, _IOLBF, 0))
        err(1, "setvbuf");
    start_scan_threads(&o, requested_scan_threads(), IDLE_BITMAP);
    observe(&o);
    stop_scan_threads(&o);
    return fflush(stdout) == EOF || ferror(stdout);
}
