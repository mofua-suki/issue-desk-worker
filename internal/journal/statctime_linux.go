package journal

import "syscall"

func statChangeTime(stat *syscall.Stat_t) syscall.Timespec { return stat.Ctim }
