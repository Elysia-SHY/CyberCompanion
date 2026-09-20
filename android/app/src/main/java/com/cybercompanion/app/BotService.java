package com.cybercompanion.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.util.Log;

import androidx.core.app.NotificationCompat;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;

public class BotService extends Service {
    private static final String TAG = "CyberCompanionService";
    private static final String CHANNEL_ID = "cybercompanion_channel";
    private static final int NOTIFICATION_ID = 1001;

    private static Process botProcess;
    private static boolean isRunning = false;
    private static String statusMessage = "初始化中...";

    // ── 崩溃自动重启 ──────────────────────────────────────────────────────
    // Go 核心进程一旦异常退出，此前只会把状态置为"已退出"然后干等，
    // 必须用户手动打开 App 才恢复 —— 这与"24 小时在线"的承诺不符（建议书 7.1 ③）。
    // 这里改为带退避的自动重启：崩溃越频繁，等待越久，避免崩溃循环打满 CPU。
    private static final int MAX_RESTART_ATTEMPTS = 5;
    private static final long RESTART_BASE_DELAY_MS = 5000L;
    private volatile boolean autoRestart = true;
    private int restartCount = 0;

    public static boolean isServiceRunning() {
        return isRunning;
    }

    public static String getStatusMessage() {
        return statusMessage;
    }

    @Override
    public void onCreate() {
        super.onCreate();
        createNotificationChannel();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        try {
            if (Build.VERSION.SDK_INT >= 34) {
                startForeground(NOTIFICATION_ID, buildNotification(), ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
            } else if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                startForeground(NOTIFICATION_ID, buildNotification(), ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
            } else {
                startForeground(NOTIFICATION_ID, buildNotification());
            }
        } catch (Throwable e) {
            Log.e(TAG, "startForeground with type failed: " + e.getMessage());
            try {
                startForeground(NOTIFICATION_ID, buildNotification());
            } catch (Throwable t) {
                Log.e(TAG, "Default startForeground failed: " + t.getMessage());
            }
        }

        if (!isRunning) {
            startGoEngine();
        }
        return START_STICKY;
    }

    private void startGoEngine() {
        isRunning = true;
        statusMessage = "正在准备核心程序...";

        new Thread(() -> {
            // 外层循环负责崩溃后的退避重启；runGoProcess 返回后决定是否继续
            while (autoRestart) {
                int exitCode = runGoProcess();
                if (exitCode == 0) {
                    // 正常退出（例如收到停止指令），不再重启
                    break;
                }
                if (restartCount >= MAX_RESTART_ATTEMPTS) {
                    statusMessage = "核心进程反复崩溃，已停止自动重启（请查看日志）";
                    Log.e(TAG, statusMessage);
                    break;
                }
                restartCount++;
                long delay = RESTART_BASE_DELAY_MS * (1L << Math.min(restartCount - 1, 4));
                statusMessage = "核心进程异常退出，" + (delay / 1000) + " 秒后第 " + restartCount + " 次重启";
                Log.w(TAG, statusMessage);
                if (!sleepQuietly(delay)) {
                    break;
                }
                if (!autoRestart) {
                    break;
                }
            }
            isRunning = false;
        }).start();
    }

    /** 返回进程退出码；启动失败时返回非 0 值以触发重启逻辑。 */
    private int runGoProcess() {
            try {
                File filesDir = getFilesDir();
                File configFile = new File(filesDir, "config.json");

                // 1. Initialize default config if not present
                if (!configFile.exists()) {
                    writeDefaultConfig(configFile);
                }

                // 2. Find or extract binary
                File binaryFile = getOrExtractBinary();
                if (binaryFile == null || !binaryFile.exists()) {
                    statusMessage = "错误：找不到适用于本机的核心二进制文件";
                    Log.e(TAG, statusMessage);
                    isRunning = false;
                    // 返回 0 表示「正常结束」：二进制缺失是持久性问题，
                    // 重启循环再试 5 次也是白费，直接交给外层退出。
                    return 0;
                }

                statusMessage = "正在启动服务进程...";
                Log.i(TAG, "Launching binary: " + binaryFile.getAbsolutePath() + " -config " + configFile.getAbsolutePath());

                ProcessBuilder pb = new ProcessBuilder(
                        binaryFile.getAbsolutePath(),
                        "-config", configFile.getAbsolutePath(),
                        "-port", "8088"
                );
                pb.directory(filesDir);
                pb.environment().put("HOME", filesDir.getAbsolutePath());
                pb.environment().put("TMPDIR", getCacheDir().getAbsolutePath());
                pb.environment().put("PATH", System.getenv("PATH") + ":" + filesDir.getAbsolutePath());
                pb.redirectErrorStream(true);

                botProcess = pb.start();
                // 连续运行超过 60 秒视为稳定，重置崩溃计数，避免历史累计导致过早放弃
                resetCrashCounterAfterGracePeriod();
                statusMessage = "核心服务已在后台运行 (端口: 8088)";

                try (BufferedReader reader = new BufferedReader(new InputStreamReader(botProcess.getInputStream()))) {
                    String line;
                    while ((line = reader.readLine()) != null) {
                        Log.d("CyberCompanionCore", line);
                    }
                } catch (Exception e) {
                    Log.w(TAG, "Process stream closed: " + e.getMessage());
                }

                int exitCode = botProcess.waitFor();
                statusMessage = "核心进程已退出 (代码: " + exitCode + ")";
                Log.w(TAG, statusMessage);
                return exitCode;

            } catch (Exception e) {
                statusMessage = "启动异常: " + e.getMessage();
                Log.e(TAG, "Failed to run Go core: ", e);
                return -1;
            }
    }

    /** 可被中断的等待；返回 false 表示等待被打断，应停止重启循环。 */
    private boolean sleepQuietly(long ms) {
        try {
            Thread.sleep(ms);
            return true;
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return false;
        }
    }

    private File getOrExtractBinary() {
        // First check nativeLibraryDir
        String nativeLibDir = getApplicationInfo().nativeLibraryDir;
        File nativeFile = new File(nativeLibDir, "libcybercompanion.so");
        if (nativeFile.exists()) {
            nativeFile.setExecutable(true, false);
            if (nativeFile.canExecute()) {
                return nativeFile;
            }
        }

        // Second: fallback to extracting from APK into app internal storage
        File binDir = new File(getFilesDir(), "bin");
        if (!binDir.exists()) {
            binDir.mkdirs();
        }
        File extractedFile = new File(binDir, "cybercompanion");
        if (extractFromApk(extractedFile)) {
            extractedFile.setExecutable(true, false);
            extractedFile.setReadable(true, false);
            try {
                Runtime.getRuntime().exec(new String[]{"chmod", "755", extractedFile.getAbsolutePath()}).waitFor();
            } catch (Exception ignored) {}
            return extractedFile;
        }

        return nativeFile.exists() ? nativeFile : null;
    }

    private boolean extractFromApk(File dest) {
        ZipFile zip = null;
        try {
            String apkPath = getApplicationInfo().sourceDir;
            zip = new ZipFile(apkPath);
            String[] preferredAbis = Build.SUPPORTED_ABIS;
            ZipEntry targetEntry = null;

            for (String abi : preferredAbis) {
                ZipEntry entry = zip.getEntry("lib/" + abi + "/libcybercompanion.so");
                if (entry != null) {
                    targetEntry = entry;
                    Log.i(TAG, "Matched ABI: " + abi);
                    break;
                }
            }

            if (targetEntry == null) {
                targetEntry = zip.getEntry("lib/arm64-v8a/libcybercompanion.so");
            }
            if (targetEntry == null) {
                targetEntry = zip.getEntry("lib/armeabi-v7a/libcybercompanion.so");
            }

            if (targetEntry == null) {
                Log.e(TAG, "No suitable library entry in APK");
                return false;
            }

            if (dest.exists() && dest.length() == targetEntry.getSize()) {
                return true;
            }

            try (InputStream is = zip.getInputStream(targetEntry);
                 FileOutputStream fos = new FileOutputStream(dest)) {
                byte[] buffer = new byte[16384];
                int len;
                while ((len = is.read(buffer)) > 0) {
                    fos.write(buffer, 0, len);
                }
                fos.flush();
            }
            Log.i(TAG, "Extracted " + targetEntry.getName() + " to " + dest.getAbsolutePath());
            return true;
        } catch (Exception e) {
            Log.e(TAG, "Extraction failed: ", e);
            return false;
        } finally {
            if (zip != null) {
                try { zip.close(); } catch (Exception ignored) {}
            }
        }
    }

    private void writeDefaultConfig(File dest) {
        // 安全说明：passcode 与 web_password 均留空，由用户首次启动后自行设置。
        // 内置固定口令等同于「任何读过源码的人都能拿到主人权限」，故不再提供默认值。
        String defaultConfig = "{\n" +
                "  \"qq_appid\": \"\",\n" +
                "  \"qq_secret\": \"\",\n" +
                "  \"oneapi_url\": \"https://api.deepseek.com/v1/chat/completions\",\n" +
                "  \"oneapi_token\": \"\",\n" +
                "  \"model\": \"deepseek-chat\",\n" +
                "  \"bot_name\": \"DEEPSEEK-CHAN\",\n" +
                "  \"system_prompt\": \"你是一个温柔贴心的二次元日常陪伴少女。\",\n" +
                "  \"active_persona\": \"deepseek_chan\",\n" +
                "  \"owners\": [],\n" +
                "  \"owners_file\": \"owners.json\",\n" +
                "  \"passcode\": \"\",\n" +
                "  \"web_port\": 8088,\n" +
                "  \"web_password\": \"\",\n" +
                "  \"trusted_proxies\": [],\n" +
                "  \"max_history_msgs\": 40,\n" +
                "  \"token_budget\": 6000,\n" +
                "  \"enable_stickers\": true,\n" +
                "  \"enable_exec\": false,\n" +
                "  \"exec_whitelist\": []\n" +
                "}";
        try (FileOutputStream fos = new FileOutputStream(dest)) {
            fos.write(defaultConfig.getBytes("UTF-8"));
            // 配置文件含面板密码与模型密钥，权限收紧为仅属主可读写
            try {
                Runtime.getRuntime().exec(new String[]{"chmod", "600", dest.getAbsolutePath()}).waitFor();
            } catch (Exception ignored) {}
            Log.i(TAG, "Default config created at: " + dest.getAbsolutePath());
        } catch (Exception e) {
            Log.e(TAG, "Error writing default config: ", e);
        }
    }

    private void createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            NotificationChannel channel = new NotificationChannel(
                    CHANNEL_ID,
                    "CyberCompanion 伴侣前台服务",
                    NotificationManager.IMPORTANCE_LOW
            );
            channel.setDescription("保持 QQ 机器人与 Web 控制台 24 小时后台运行");
            NotificationManager manager = getSystemService(NotificationManager.class);
            if (manager != null) {
                manager.createNotificationChannel(channel);
            }
        }
    }

    private Notification buildNotification() {
        Intent notificationIntent = new Intent(this, MainActivity.class);
        PendingIntent pendingIntent = PendingIntent.getActivity(
                this, 0, notificationIntent,
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT
        );

        return new NotificationCompat.Builder(this, CHANNEL_ID)
                .setContentTitle("CyberCompanion 运行中")
                .setContentText("QQ 机器人服务与本地 Web 控制台 (:8088) 在线")
                .setSmallIcon(R.drawable.ic_launcher)
                .setContentIntent(pendingIntent)
                .setOngoing(true)
                .build();
    }

    /**
     * 进程稳定运行一段时间后清零崩溃计数。
     * 这样"偶尔一次崩溃"不会累积到上限，只有真正的连续崩溃才会停止重启。
     */
    private void resetCrashCounterAfterGracePeriod() {
        new Thread(() -> {
            if (!sleepQuietly(60000L)) {
                return;
            }
            synchronized (BotService.class) {
                if (isRunning) {
                    restartCount = 0;
                }
            }
        }).start();
    }

    @Override
    public void onDestroy() {
        // 服务被系统或用户销毁时停止自动重启，避免"杀不掉"的副作用
        autoRestart = false;
        if (botProcess != null) {
            botProcess.destroy();
        }
        isRunning = false;
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
