package com.cybercompanion.app;

import android.app.job.JobInfo;
import android.app.job.JobParameters;
import android.app.job.JobScheduler;
import android.app.job.JobService;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.os.Build;
import android.util.Log;

/**
 * 兜底拉活服务。
 *
 * 前台服务在国产 ROM（尤其是随身 WiFi 这类精简系统）上仍可能被系统清理。
 * JobScheduler 由系统统一调度，是官方认可的「重启后 / 长期空闲后」恢复入口，
 * 比单独依赖 START_STICKY 更可靠。
 */
public class KeepAliveJobService extends JobService {
    private static final String TAG = "CCKeepAlive";
    private static final int JOB_ID = 8801;
    private static final long INTERVAL_MS = 15 * 60 * 1000L; // 15 分钟

    /** 在 Application / BootReceiver / MainActivity 中调用，注册兜底任务 */
    public static void schedule(Context context) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.LOLLIPOP) {
            return;
        }
        try {
            JobScheduler scheduler =
                    (JobScheduler) context.getSystemService(Context.JOB_SCHEDULER_SERVICE);
            if (scheduler == null) {
                return;
            }

            ComponentName component = new ComponentName(context, KeepAliveJobService.class);
            JobInfo.Builder builder = new JobInfo.Builder(JOB_ID, component)
                    .setPersisted(true)              // 重启后仍然有效
                    .setPeriodic(INTERVAL_MS)
                    .setRequiredNetworkType(JobInfo.NETWORK_TYPE_ANY);

            // 低电量时也允许执行，避免设备长期插电但电量被限制
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                builder.setRequiresBatteryNotLow(false);
                builder.setRequiresCharging(false);
            }

            int result = scheduler.schedule(builder.build());
            Log.i(TAG, "兜底拉活任务注册结果: " + result);
        } catch (Throwable e) {
            Log.w(TAG, "注册 JobScheduler 失败", e);
        }
    }

    @Override
    public boolean onStartJob(JobParameters params) {
        Log.i(TAG, "兜底任务触发，检查核心服务状态");
        try {
            if (!BotService.isServiceRunning()) {
                Log.i(TAG, "核心服务未运行，尝试拉起");
                Intent intent = new Intent(this, BotService.class);
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                    startForegroundService(intent);
                } else {
                    startService(intent);
                }
            }
        } catch (Throwable e) {
            Log.e(TAG, "拉起核心服务失败", e);
        }
        // 返回 false：任务已同步完成，不需要额外的后台线程
        return false;
    }

    @Override
    public boolean onStopJob(JobParameters params) {
        // 返回 true 表示被中断后希望重新调度
        return true;
    }
}
