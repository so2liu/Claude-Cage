// Per-user guest application. macOS grants only the project selected in the
// native Open panel. Terminal and editor children inherit that user selection.
#import <AppKit/AppKit.h>
#import <fcntl.h>
#import <poll.h>
#import <signal.h>
#import <spawn.h>
#import <sys/file.h>
#import <sys/ioctl.h>
#import <sys/mount.h>
#import <sys/socket.h>
#import <sys/stat.h>
#import <sys/un.h>
#import <sys/wait.h>
#import <termios.h>
#import <unistd.h>

extern char **environ;
static NSString *socketPath(void) {
    return [NSHomeDirectory()
        stringByAppendingPathComponent:@"Library/Application Support/Cage/project-terminal.sock"];
}
static BOOL addressForSocket(struct sockaddr_un *address) {
    const char *p = socketPath().fileSystemRepresentation;
    memset(address, 0, sizeof(*address));
    address->sun_family = AF_UNIX;
    return strlcpy(address->sun_path, p, sizeof(address->sun_path)) < sizeof(address->sun_path);
}
static BOOL writeAll(int fd, const void *data, size_t count) {
    const char *p = data;
    while (count) {
        ssize_t n = write(fd, p, count);
        if (n < 0 && errno == EINTR)
            continue;
        if (n <= 0)
            return NO;
        p += n;
        count -= n;
    }
    return YES;
}
static void reply(int fd, const char *message) { writeAll(fd, message, strlen(message)); }
static NSString *mountID(NSString *project) {
    if (![project isKindOfClass:NSString.class] || ![project.stringByStandardizingPath isEqual:project])
        return nil;
    NSString *base = [NSHomeDirectory() stringByAppendingPathComponent:@"Cage/Projects"];
    NSString *leaf = project.lastPathComponent;
    if (![project.stringByDeletingLastPathComponent isEqual:base] || leaf.length != 24 ||
        [leaf rangeOfCharacterFromSet:[NSCharacterSet characterSetWithCharactersInString:@"0123456789abcdef"]
                                          .invertedSet]
                .location != NSNotFound)
        return nil;
    struct statfs fs;
    if (statfs(project.fileSystemRepresentation, &fs) ||
        strcmp(fs.f_mntonname, project.fileSystemRepresentation) || strcmp(fs.f_fstypename, "macfuse") ||
        fs.f_owner != getuid() || (fs.f_flags & (MNT_NOSUID | MNT_NODEV)) != (MNT_NOSUID | MNT_NODEV))
        return nil;
    return [NSString stringWithUTF8String:fs.f_mntfromname];
}

@interface ProjectAccess : NSObject <NSApplicationDelegate, NSOpenSavePanelDelegate>
@end
@implementation ProjectAccess {
    NSMutableDictionary<NSString *, NSURL *> *_scopes;
    NSMutableDictionary<NSString *, NSString *> *_ready;
    NSString *_requesting;
    NSString *_nonce;
    NSOpenPanel *_panel;
    NSMutableDictionary<NSString *, NSDate *> *_declined;
}
- (void)applicationDidFinishLaunching:(NSNotification *)note {
    _scopes = [NSMutableDictionary dictionary];
    _ready = [NSMutableDictionary dictionary];
    _declined = [NSMutableDictionary dictionary];
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
      [self serve];
    });
}
// These state and UI methods run on the main thread.
- (BOOL)restore:(NSString *)project {
    if (_scopes[project])
        return YES;
    NSData *saved = [[NSUserDefaults standardUserDefaults] dataForKey:project];
    if (!saved)
        return NO;
    BOOL stale = NO;
    NSURL *url = [NSURL
        URLByResolvingBookmarkData:saved
                           options:NSURLBookmarkResolutionWithSecurityScope |
                                   NSURLBookmarkResolutionWithoutUI | NSURLBookmarkResolutionWithoutMounting
                     relativeToURL:nil
               bookmarkDataIsStale:&stale
                             error:nil];
    if (![url.path isEqual:project] || ![url startAccessingSecurityScopedResource])
        return NO;
    _scopes[project] = url;
    if (stale)
        [self remember:url];
    return YES;
}
- (void)remember:(NSURL *)url {
    NSData *bookmark = [url bookmarkDataWithOptions:NSURLBookmarkCreationWithSecurityScope
                     includingResourceValuesForKeys:nil
                                      relativeToURL:nil
                                              error:nil];
    if (bookmark)
        [[NSUserDefaults standardUserDefaults] setObject:bookmark forKey:url.path];
}
- (void)request:(NSString *)project {
    if (_requesting || (_declined[project] && [_declined[project] timeIntervalSinceNow] > 0))
        return;
    _requesting = project;
    NSOpenPanel *panel = [NSOpenPanel openPanel];
    panel.delegate = self;
    panel.canChooseDirectories = YES;
    panel.canChooseFiles = NO;
    panel.allowsMultipleSelection = NO;
    panel.directoryURL = [NSURL fileURLWithPath:project isDirectory:YES];
    panel.message = @"Allow Cage to open this mounted project in the guest terminal and editor.";
    panel.prompt = @"Use Project";
    _nonce = NSUUID.UUID.UUIDString;
    _panel = panel;
    __block NSTimer *timer;
    [NSApp activateIgnoringOtherApps:YES];
    [panel beginWithCompletionHandler:^(NSModalResponse response) {
      [timer invalidate];
      self->_panel = nil;
      self->_nonce = nil;
      NSURL *url = panel.URL;
      if (response == NSModalResponseOK && [url.path isEqual:project] && mountID(project) &&
          [url startAccessingSecurityScopedResource]) {
          self->_scopes[project] = url;
          [self remember:url];
      } else {
          self->_declined[project] = [NSDate dateWithTimeIntervalSinceNow:30];
      }
      self->_requesting = nil;
    }];
    // The request must disappear if the mount is revoked while the panel is open.
    timer =
        [NSTimer scheduledTimerWithTimeInterval:.25
                                        repeats:YES
                                          block:^(NSTimer *t) {
                                            if (!mountID(project) || ![self->_requesting isEqual:project]) {
                                                [panel cancel:nil];
                                                return;
                                            }
                                          }];
}
- (NSString *)probe:(NSDictionary *)req {
    NSString *project = req[@"path"], *file = req[@"file"], *expected = req[@"expected"],
             *identity = mountID(project);
    if (!identity || ![file isKindOfClass:NSString.class] || ![file hasPrefix:@".cage-mount-probe-"] ||
        ![file.lastPathComponent isEqual:file] || ![expected isKindOfClass:NSString.class] ||
        expected.length < 1 || expected.length > 256)
        return @"ERR";
    if (_ready[project] && ![_ready[project] isEqual:identity]) {
        [_ready removeObjectForKey:project];
        [_scopes[project] stopAccessingSecurityScopedResource];
        [_scopes removeObjectForKey:project];
    }
    if (![self restore:project]) {
        [self request:project];
        return @"PENDING";
    }
    NSString *filePath = [project stringByAppendingPathComponent:file];
    // Bounded read of the host challenge. A link cannot be used for the proof.
    char b[1024];
    int fd = open(filePath.fileSystemRepresentation, O_RDONLY | O_NOFOLLOW | O_CLOEXEC | O_NONBLOCK);
    if (fd < 0) {
        if (errno == EPERM || errno == EACCES) {
            [_ready removeObjectForKey:project];
            [_scopes[project] stopAccessingSecurityScopedResource];
            [_scopes removeObjectForKey:project];
            [self request:project];
            return @"PENDING";
        }
        return @"ERR";
    }
    struct stat st;
    ssize_t n =
        !fstat(fd, &st) && S_ISREG(st.st_mode) && st.st_size < sizeof(b) ? read(fd, b, sizeof(b)) : -1;
    close(fd);
    NSData *answer = [expected dataUsingEncoding:NSUTF8StringEncoding];
    if (n < 0 || n != (ssize_t)answer.length || memcmp(b, answer.bytes, n) ||
        ![identity isEqual:mountID(project)])
        return @"ERR";
    _ready[project] = identity;
    return @"OK";
}
- (void)serve {
    struct sockaddr_un address;
    if (!addressForSocket(&address))
        return;
    NSString *dir = socketPath().stringByDeletingLastPathComponent;
    NSFileManager *fm = [NSFileManager defaultManager];
    if (![fm createDirectoryAtPath:dir
            withIntermediateDirectories:YES
                             attributes:@{NSFilePosixPermissions : @0700}
                                  error:nil])
        return;
    struct stat st;
    // Other Cage components may have created this shared parent with mode 0755.
    // Tighten only a real directory owned by this user before binding the socket.
    int directory = open(dir.fileSystemRepresentation, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
    if (directory < 0)
        return;
    BOOL privateDirectory = !fstat(directory, &st) && S_ISDIR(st.st_mode) && st.st_uid == getuid() &&
                            (!(st.st_mode & 0077) || !fchmod(directory, 0700));
    close(directory);
    if (!privateDirectory)
        return;
    // The advisory lock distinguishes a dead listener from another running copy.
    NSString *l = [dir stringByAppendingPathComponent:@"project-terminal.lock"];
    int lock = open(l.fileSystemRepresentation, O_RDWR | O_CREAT | O_CLOEXEC | O_NOFOLLOW, 0600);
    if (lock < 0 || flock(lock, LOCK_EX | LOCK_NB)) {
        if (lock >= 0)
            close(lock);
        return;
    }
    if (!lstat(address.sun_path, &st) && !(S_ISSOCK(st.st_mode) && st.st_uid == getuid())) {
        close(lock);
        return;
    }
    unlink(address.sun_path);
    int listener = socket(AF_UNIX, SOCK_STREAM, 0);
    if (listener < 0 || bind(listener, (struct sockaddr *)&address, sizeof(address)) ||
        chmod(address.sun_path, 0600) || listen(listener, 12)) {
        if (listener >= 0)
            close(listener);
        close(lock);
        return;
    }
    fcntl(listener, F_SETFD, FD_CLOEXEC);
    for (;;) {
        int fd = accept(listener, NULL, NULL);
        if (fd < 0) {
            if (errno == EINTR)
                continue;
            break;
        }
        fcntl(fd, F_SETFD, FD_CLOEXEC);
        dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
          @autoreleasepool {
              [self handle:fd];
              close(fd);
          }
        });
    }
    close(listener);
    unlink(address.sun_path);
    close(lock);
}
- (void)handle:(int)fd {
    uid_t uid;
    gid_t gid;
    if (getpeereid(fd, &uid, &gid) || uid != getuid())
        return;
    struct timeval timeout = {4, 0};
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    char bytes[8192], ancillary[CMSG_SPACE(8 * sizeof(int))];
    int tty = -1;
    size_t len = 0;
    BOOL extra = NO;
    struct iovec iov = {bytes, sizeof(bytes)};
    struct msghdr msg = {0};
    msg.msg_iov = &iov;
    msg.msg_iovlen = 1;
    msg.msg_control = ancillary;
    msg.msg_controllen = sizeof(ancillary);
    ssize_t n = recvmsg(fd, &msg, 0);
    if (n <= 0)
        return;
    for (struct cmsghdr *c = CMSG_FIRSTHDR(&msg); c; c = CMSG_NXTHDR(&msg, c)) {
        if (c->cmsg_level != SOL_SOCKET || c->cmsg_type != SCM_RIGHTS)
            continue;
        size_t count = (c->cmsg_len - CMSG_LEN(0)) / sizeof(int);
        int *fds = (int *)CMSG_DATA(c);
        for (size_t i = 0; i < count; i++) {
            if (tty < 0) {
                tty = fds[i];
                fcntl(tty, F_SETFD, FD_CLOEXEC);
            } else {
                close(fds[i]);
                extra = YES;
            }
        }
    }
    @try {
        if (extra || (msg.msg_flags & MSG_CTRUNC))
            return;
        len = (size_t)n;
        while (!memchr(bytes, '\n', len) && len < sizeof(bytes)) {
            n = read(fd, bytes + len, sizeof(bytes) - len);
            if (n <= 0)
                return;
            len += n;
        }
        if (bytes[len - 1] != '\n')
            return;
        NSDictionary *req = [NSJSONSerialization JSONObjectWithData:[NSData dataWithBytes:bytes length:len]
                                                            options:0
                                                              error:nil];
        if (![req isKindOfClass:NSDictionary.class])
            return;
        NSString *project = req[@"path"], *action = req[@"action"];
        if (!mountID(project)) {
            reply(fd, "ERR\n");
            return;
        }
        // The authenticated window forwarder obtains the live NSWindow from the
        // process that opened the panel. Closing it invalidates the nonce.
        if ([action isEqual:@"panel"] && tty < 0) {
            __block NSDictionary *state;
            dispatch_sync(dispatch_get_main_queue(), ^{
              if ([self->_requesting isEqual:project] && self->_nonce && self->_panel.visible &&
                  self->_panel.windowNumber > 0 &&
                  [self->_panel.directoryURL.URLByStandardizingPath.path isEqual:project])
                  state = @{
                      @"path" : project,
                      @"nonce" : self->_nonce,
                      @"pid" : @(getpid()),
                      @"windowNumber" : @(self->_panel.windowNumber)
                  };
            });
            if (!state) {
                reply(fd, "ERR\n");
                return;
            }
            NSData *json = [NSJSONSerialization dataWithJSONObject:state options:0 error:nil];
            writeAll(fd, json.bytes, json.length);
            reply(fd, "\n");
            return;
        }
        if ([action isEqual:@"probe"] && tty < 0) {
            __block NSString *status;
            dispatch_sync(dispatch_get_main_queue(), ^{
              status = [self probe:req];
            });
            reply(fd, [status stringByAppendingString:@"\n"].UTF8String);
            return;
        }
        BOOL claude = [action isEqual:@"claude"];
        NSString *launch = req[@"launch"];
        if (tty < 0 || !([action isEqual:@"terminal"] || [action isEqual:@"editor"] || claude) ||
            (claude && (![launch isKindOfClass:NSString.class] || !launch.length ||
                        [launch lengthOfBytesUsingEncoding:NSUTF8StringEncoding] > 4096))) {
            reply(fd, "ERR\n");
            return;
        }
        struct stat st;
        char name[PATH_MAX];
        if (fstat(tty, &st) || !S_ISCHR(st.st_mode) || st.st_uid != uid ||
            ttyname_r(tty, name, sizeof(name)) || strncmp(name, "/dev/ttys", 9) || !isatty(tty)) {
            reply(fd, "ERR\n");
            return;
        }
        __block NSString *identity;
        dispatch_sync(dispatch_get_main_queue(), ^{
          identity = self->_ready[project];
        });
        if (!identity || ![identity isEqual:mountID(project)]) {
            reply(fd, "ERR\n");
            return;
        }
        NSString *exe = NSBundle.mainBundle.executableURL.path;
        char *argv[] = {(char *)exe.fileSystemRepresentation, "--child",
                        (char *)project.fileSystemRepresentation, (char *)action.UTF8String,
                        claude ? (char *)launch.UTF8String : NULL, NULL};
        posix_spawn_file_actions_t files;
        posix_spawn_file_actions_init(&files);
        posix_spawn_file_actions_adddup2(&files, tty, 3);
        posix_spawnattr_t attr;
        posix_spawnattr_init(&attr);
        posix_spawnattr_setflags(&attr, POSIX_SPAWN_CLOEXEC_DEFAULT);
        pid_t pid;
        int err = posix_spawn(&pid, argv[0], &files, &attr, argv, environ);
        posix_spawnattr_destroy(&attr);
        posix_spawn_file_actions_destroy(&files);
        if (err) {
            reply(fd, "ERR\n");
            return;
        }
        reply(fd, "OK\n");
        for (;;) {
            int childStatus;
            pid_t done = waitpid(pid, &childStatus, WNOHANG);
            if (done == pid || (done < 0 && errno != EINTR))
                break;
            struct pollfd pollfd = {fd, POLLIN, 0};
            if (poll(&pollfd, 1, 500) > 0 || ![identity isEqual:mountID(project)]) {
                kill(pid, SIGHUP);
                kill(-pid, SIGHUP);
                for (int i = 0; i < 15; i++) {
                    if (waitpid(pid, &childStatus, WNOHANG) == pid)
                        return;
                    usleep(20000);
                }
                kill(pid, SIGKILL);
                kill(-pid, SIGKILL);
                waitpid(pid, &childStatus, 0);
                break;
            }
        }
    } @finally {
        if (tty >= 0)
            close(tty);
    }
}
- (void)panel:(id)sender didChangeToDirectoryURL:(NSURL *)url {
    // The host offered exactly one mounted project, and must never preview
    // guest directories reached through the dialog's sidebar.
    if (sender == _panel && _requesting && url && ![url.URLByStandardizingPath.path isEqual:_requesting])
        [_panel cancel:nil];
}
@end

static int probeCLI(int argc, char **argv) {
    if (argc != 5)
        return 2;
    struct sockaddr_un address;
    if (!addressForSocket(&address))
        return 1;
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0)
        return 1;
    struct timeval wait = {4, 0};
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &wait, sizeof(wait));
    if (connect(fd, (struct sockaddr *)&address, sizeof(address))) {
        close(fd);
        return 1;
    }
    uid_t peer;
    gid_t group;
    if (getpeereid(fd, &peer, &group) || peer != getuid()) {
        close(fd);
        return 1;
    }
    NSData *j = [NSJSONSerialization dataWithJSONObject:@{
        @"action" : @"probe",
        @"path" : @(argv[2]),
        @"file" : @(argv[3]),
        @"expected" : @(argv[4])
    }
                                                options:0
                                                  error:nil];
    BOOL ok = writeAll(fd, j.bytes, j.length) && writeAll(fd, "\n", 1);
    FILE *f = fdopen(fd, "r");
    if (!f) {
        close(fd);
        return 1;
    }
    char line[32] = {0};
    if (!ok || !fgets(line, sizeof(line), f)) {
        fclose(f);
        return 1;
    }
    fclose(f);
    if (!strcmp(line, "OK\n")) {
        puts("OK");
        return 0;
    }
    if (!strcmp(line, "PENDING\n")) {
        puts("PENDING");
        return 75;
    }
    return 1;
}

int main(int argc, char **argv) {
    @autoreleasepool {
        if (argc > 1 && !strcmp(argv[1], "probe"))
            return probeCLI(argc, argv);
        if ((argc == 4 || (argc == 5 && !strcmp(argv[3], "claude"))) && !strcmp(argv[1], "--child")) {
            if (!mountID(@(argv[2])) || setsid() < 0 || ioctl(3, TIOCSCTTY, 0) < 0)
                return 1;
            for (int i = 0; i < 3; i++)
                if (dup2(3, i) < 0)
                    return 1;
            close(3);
            signal(SIGPIPE, SIG_DFL);
            if (chdir(argv[2]))
                return 1;
            setenv("TERM", "xterm-256color", 1);
            setenv("PWD", argv[2], 1);
            setenv("PATH", "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", 1);
            if (argc == 5) {
                execl("/usr/local/libexec/cage-cli/bin/python3", "python3", "-I",
                      "/usr/local/libexec/cage-cli/macos-cli.py", "macos-launch", argv[4], (char *)NULL);
                return 1;
            }
            if (!strcmp(argv[3], "terminal"))
                execl("/bin/zsh", "-zsh", (char *)NULL);
            if (!strcmp(argv[3], "editor"))
                execl("/usr/local/libexec/cage-macos-editor", "cage-macos-editor", (char *)NULL);
            return 1;
        }
        if (argc != 1)
            return 2;
        signal(SIGPIPE, SIG_IGN);
        umask(0077);
        NSApplication *app = NSApplication.sharedApplication;
        ProjectAccess *delegate = [[ProjectAccess alloc] init];
        app.delegate = delegate;
        [app setActivationPolicy:NSApplicationActivationPolicyAccessory];
        [app run];
        return 0;
    }
}
