package com.cybercompanion.app;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.os.Build;
import android.util.Log;

public class BootReceiver extends BroadcastReceiver {
    private static final String TAG = "CyberCompanionBoot";

    @Override
    public void onReceive(Context context, Intent intent) {
        if (Intent.ACTION_BOOT_COMPLETED.equals(intent.getAction())) {
            Log.i(TAG, "Device boot completed, starting CyberCompanion service...");
            // 开机后重新注册兜底任务（setPersisted 在部分精简 ROM 上不生效，
            // 开机广播里再注册一次可以覆盖这种情况）
            KeepAliveJobService.schedule(context);
            Intent serviceIntent = new Intent(context, BotService.class);
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(serviceIntent);
            } else {
                context.startService(serviceIntent);
            }
        }
    }
}
