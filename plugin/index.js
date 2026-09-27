// Hydro 插件：在 TMOE Debian 容器里，用 watcher 监控评测
// 状态码：0 SystemError / 1 Accepted / 2 WrongAnswer / 3 RuntimeError
//        4 TimeLimitExceeded / 5 MemoryLimitExceeded

const fs = require('fs');
const { execa } = require('execa');

// ===== 你可以改这里 =====
// watcher 可执行文件的绝对路径（在 TMOE Debian 内部）
const WATCHER_PATH =
  process.env.WATCHER_PATH ||
  '/root/hydro-proot-judge/watcher/watcher';

// 临时文件目录（TMOE Debian 内部的 /tmp 就行）
const TMP_DIR = process.env.TMP_DIR || '/tmp';
// ========================

const STATUS = {
  SystemError: 0,
  Accepted: 1,
  WrongAnswer: 2,
  RuntimeError: 3,
  TimeLimitExceeded: 4,
  MemoryLimitExceeded: 5,
};

async function apply(ctx) {
  ctx.on('judge/execute', async (task, next) => {
    const execCmd = task && task.config && task.config.execute;
    if (!execCmd) {
      return next();
    }

    const timeLimitSec = (task.config.time || 1000) / 1000;
    const memLimitKb = (task.config.memory || 256) * 1024;
    const procLimit = task.config.procLimit || 30;

    const stamp = `${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
    const outFile = `${TMP_DIR}/hydro_judge_${stamp}.out`;
    const errFile = `${TMP_DIR}/hydro_judge_${stamp}.err`;

    const args = [
      '-mem', String(memLimitKb),
      '-proc', String(procLimit),
      '-time', String(timeLimitSec),
      // 注意：这里不传 -proot，watcher 会直接执行 -exec
      '-exec', execCmd,
      '-out', outFile,
      '-err', errFile,
    ];

    ctx.logger.info('[proot-judge] 调用 watcher:', WATCHER_PATH, args.join(' '));

    let stdout = '';
    try {
      const r = await execa(WATCHER_PATH, args, {
        timeout: (timeLimitSec * 1.1 + 5) * 1000,
        reject: false,
      });
      stdout = r.stdout || '';
    } catch (e) {
      ctx.logger.error('[proot-judge] execa 调用失败:', e);
      return { status: STATUS.SystemError, score: 0 };
    }

    const match = stdout.match(/RESULT:(\w+)/);
    const status = match ? match[1] : 'SystemError';
    const exitMatch = stdout.match(/EXIT_CODE:(\d+)/);
    const exitCode = exitMatch ? parseInt(exitMatch[1], 10) : 0;

    let userOutput = '';
    try {
      userOutput = fs.readFileSync(outFile, 'utf8');
    } catch (e) {
      userOutput = '';
    }
    try { fs.unlinkSync(outFile); } catch (e) {}
    try { fs.unlinkSync(errFile); } catch (e) {}

    switch (status) {
      case 'Accepted':
        return { status: STATUS.Accepted, score: 100, output: userOutput };
      case 'TimeLimitExceeded':
        return { status: STATUS.TimeLimitExceeded, score: 0, output: userOutput };
      case 'MemoryLimitExceeded':
        return { status: STATUS.MemoryLimitExceeded, score: 0, output: userOutput };
      case 'RuntimeError':
        return { status: STATUS.RuntimeError, score: 0, output: userOutput, exitCode };
      default:
        ctx.logger.error('[proot-judge] watcher 输出异常:', stdout);
        return { status: STATUS.SystemError, score: 0 };
    }
  });
}

module.exports = { apply };
